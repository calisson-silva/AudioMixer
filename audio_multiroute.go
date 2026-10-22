//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/degubites/go-wca/pkg/wca"
	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

func applyPCMVolume(data []byte, volume float64, format pcmFormat) {
	if volume < 0 {
		volume = 0
	}
	if volume > 1 {
		volume = 1
	}
	for i := 0; i+format.sampleSize <= len(data); i += format.sampleSize {
		if format.encoding == waveFormatIEEEFloat && format.sampleBits == 32 {
			v := math.Float32frombits(binary.LittleEndian.Uint32(data[i:]))
			binary.LittleEndian.PutUint32(data[i:], math.Float32bits(float32(float64(v)*volume)))
			continue
		}
		if format.encoding == waveFormatIEEEFloat && format.sampleBits == 64 {
			v := math.Float64frombits(binary.LittleEndian.Uint64(data[i:]))
			binary.LittleEndian.PutUint64(data[i:], math.Float64bits(v*volume))
			continue
		}
		if format.encoding != waveFormatPCM {
			continue
		}
		switch format.sampleBits {
		case 8:
			v := int(data[i]) - 128
			n := int(float64(v) * volume)
			if n > 127 {
				n = 127
			}
			if n < -128 {
				n = -128
			}
			data[i] = byte(n + 128)
		case 16:
			v := int32(int16(binary.LittleEndian.Uint16(data[i:])))
			n := int32(float64(v) * volume)
			if n > 32767 {
				n = 32767
			}
			if n < -32768 {
				n = -32768
			}
			binary.LittleEndian.PutUint16(data[i:], uint16(int16(n)))
		case 24:
			v := int32(data[i]) | int32(data[i+1])<<8 | int32(data[i+2])<<16
			if v&0x800000 != 0 {
				v |= ^0xffffff
			}
			n := int64(float64(v) * volume)
			if n > 8388607 {
				n = 8388607
			}
			if n < -8388608 {
				n = -8388608
			}
			data[i] = byte(n)
			data[i+1] = byte(n >> 8)
			data[i+2] = byte(n >> 16)
		case 32:
			v := int64(int32(binary.LittleEndian.Uint32(data[i:])))
			n := int64(float64(v) * volume)
			if n > math.MaxInt32 {
				n = math.MaxInt32
			}
			if n < math.MinInt32 {
				n = math.MinInt32
			}
			binary.LittleEndian.PutUint32(data[i:], uint32(int32(n)))
		}
	}
}

type audioOutputConfig struct {
	deviceID string
	delay    time.Duration
	volume   float64
}

type audioOutput struct {
	config       audioOutputConfig
	client       *wca.IAudioClient
	render       *wca.IAudioRenderClient
	event        windows.Handle
	frames       uint32
	targetFrames int
	queue        *byteQueue
	resampler    adaptiveResampler
	started      bool
}

func mirrorAudio(sourceID string, configs []audioOutputConfig, stop <-chan struct{}) (err error) {
	if len(configs) == 0 {
		return errors.New("selecione pelo menos uma saída física")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		return err
	}
	defer ole.CoUninitialize()

	var enumerator *wca.IMMDeviceEnumerator
	if err = wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return err
	}
	defer enumerator.Release()

	source, err := findDevice(enumerator, sourceID)
	if err != nil {
		return err
	}
	defer source.Release()
	var captureClient *wca.IAudioClient
	if err = source.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &captureClient); err != nil {
		return fmt.Errorf("ativação da interface virtual padrão: %w", err)
	}
	defer captureClient.Release()
	var format *wca.WAVEFORMATEX
	if err = captureClient.GetMixFormat(&format); err != nil {
		return err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
	if format.NBlockAlign == 0 || format.NAvgBytesPerSec == 0 || format.NSamplesPerSec == 0 {
		return errors.New("formato PCM da interface virtual inválido")
	}
	pcm, adaptive := newPCMFormat(format)
	if !adaptive {
		return fmt.Errorf("formato da interface virtual não suportado para sincronização adaptativa: tag=%d bits=%d canais=%d", format.WFormatTag, format.WBitsPerSample, format.NChannels)
	}

	captureFlags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK | wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	captureFrames, err := initializeSharedStream(captureClient, format, captureFlags)
	if err != nil {
		return fmt.Errorf("captura da interface virtual: %w", err)
	}
	captureEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("criação do evento de captura: %w", err)
	}
	defer windows.CloseHandle(captureEvent)
	if err = captureClient.SetEventHandle(uintptr(captureEvent)); err != nil {
		return err
	}
	var capture *wca.IAudioCaptureClient
	if err = captureClient.GetService(wca.IID_IAudioCaptureClient, &capture); err != nil {
		return err
	}
	defer capture.Release()

	outputs := make([]*audioOutput, 0, len(configs))
	defer func() {
		for _, output := range outputs {
			if output.started {
				_ = output.client.Stop()
			}
			if output.event != 0 {
				_ = windows.CloseHandle(output.event)
			}
			if output.render != nil {
				output.render.Release()
			}
			if output.client != nil {
				output.client.Release()
			}
		}
	}()

	var captureLatency wca.REFERENCE_TIME
	_ = captureClient.GetStreamLatency(&captureLatency)
	for _, config := range configs {
		target, findErr := findDevice(enumerator, config.deviceID)
		if findErr != nil {
			return findErr
		}
		output := &audioOutput{config: config}
		outputs = append(outputs, output)
		if err = target.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &output.client); err != nil {
			target.Release()
			return fmt.Errorf("ativação da saída selecionada: %w", err)
		}
		target.Release()
		flags := uint32(wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
		output.frames, err = initializeSharedStream(output.client, format,
			flags|wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM,
			flags|wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM|wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
			flags)
		if err != nil {
			return fmt.Errorf("inicialização da saída selecionada: %w", err)
		}
		output.event, err = windows.CreateEvent(nil, 0, 0, nil)
		if err != nil {
			return fmt.Errorf("criação do evento de saída: %w", err)
		}
		if err = output.client.SetEventHandle(uintptr(output.event)); err != nil {
			return err
		}
		if err = output.client.GetService(wca.IID_IAudioRenderClient, &output.render); err != nil {
			return err
		}
		period := int(max(captureFrames, output.frames))
		delayFrames := int(uint64(format.NSamplesPerSec) * uint64(config.delay) / uint64(time.Second))
		output.targetFrames = period*2 + delayFrames
		capacityFrames := output.targetFrames + int(format.NSamplesPerSec)/2
		output.queue = &byteQueue{max: capacityFrames * int(format.NBlockAlign)}
		var latency wca.REFERENCE_TIME
		_ = output.client.GetStreamLatency(&latency)
		logStartup(fmt.Sprintf("output endpoint: frames=%d latency=%0.2fms delay=%dms targetQueue=%d",
			output.frames, float64(latency)/10000, config.delay.Milliseconds(), output.targetFrames))
	}

	if err = captureClient.Start(); err != nil {
		return err
	}
	defer captureClient.Stop()
	logStartup(fmt.Sprintf("virtual default loopback: %d Hz, %d channels, %d-bit, capture period=%d, %d physical output(s)",
		format.NSamplesPerSec, format.NChannels, format.WBitsPerSample, captureFrames, len(outputs)))

	processCapture := func() error {
		return copyCaptureToOutputs(capture, outputs, pcm)
	}
	processOutput := func(output *audioOutput) error {
		return renderQueuedAudioSingleLegacy(output.client, output.render, output.queue, output.frames,
			int(format.NBlockAlign), pcm, int(format.NSamplesPerSec), output.targetFrames,
			&output.resampler, true)
	}
	events := make([]windows.Handle, 1, 1+len(outputs))
	events[0] = captureEvent
	for _, output := range outputs {
		events = append(events, output.event)
	}
	allOutputsPrimed := func() bool {
		for _, output := range outputs {
			if output.queue.frames(int(format.NBlockAlign)) < output.targetFrames {
				return false
			}
		}
		return true
	}
	// Loopback endpoints may not signal while the source is silent. Do not keep
	// the UI stuck in "starting" forever: after a short prime window, start the
	// render clients and let the normal render path output silence until data
	// arrives.
	primeDeadline := time.Now().Add(1500 * time.Millisecond)
	for !allOutputsPrimed() && time.Now().Before(primeDeadline) {
		select {
		case <-stop:
			return nil
		default:
		}
		event, waitErr := windows.WaitForMultipleObjects(events, false, 25)
		if waitErr != nil {
			return fmt.Errorf("espera pelo pré-buffer: %w", waitErr)
		}
		if event == uint32(windows.WAIT_OBJECT_0) {
			if err = processCapture(); err != nil {
				return err
			}
		}
	}
	// Start every endpoint from one shared capture instant. Queues that reached
	// their target early are trimmed to their own delay, preserving their offset.
	for _, output := range outputs {
		output.queue.trimToFrames(output.targetFrames, int(format.NBlockAlign))
	}
	for _, output := range outputs {
		if err = output.client.Start(); err != nil {
			return fmt.Errorf("início de uma saída física após sincronização inicial: %w", err)
		}
		output.started = true
	}

	processSignalled := func(index int) error {
		if index == 0 {
			return processCapture()
		}
		return processOutput(outputs[index-1])
	}
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		event, waitErr := windows.WaitForMultipleObjects(events, false, 50)
		if waitErr != nil {
			return fmt.Errorf("espera pelos dispositivos de áudio: %w", waitErr)
		}
		if event == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		if event < uint32(windows.WAIT_OBJECT_0) || event >= uint32(windows.WAIT_OBJECT_0)+uint32(len(events)) {
			return fmt.Errorf("evento de áudio inesperado: %d", event)
		}
		if err = processSignalled(int(event - uint32(windows.WAIT_OBJECT_0))); err != nil {
			return err
		}
		// Drain all other signalled endpoints so one fast device cannot starve
		// the capture queue or the buffers belonging to another output.
		for index, handle := range events {
			if index == int(event-uint32(windows.WAIT_OBJECT_0)) {
				continue
			}
			ready, waitErr := windows.WaitForSingleObject(handle, 0)
			if waitErr != nil {
				return waitErr
			}
			if ready == uint32(windows.WAIT_OBJECT_0) {
				if err = processSignalled(index); err != nil {
					return err
				}
			}
		}
	}
}

func copyCaptureToOutputs(capture *wca.IAudioCaptureClient, outputs []*audioOutput, format pcmFormat) error {
	for {
		var packetFrames uint32
		if err := capture.GetNextPacketSize(&packetFrames); err != nil {
			return err
		}
		if packetFrames == 0 {
			return nil
		}
		var data *byte
		var frames, flags uint32
		if err := capture.GetBuffer(&data, &frames, &flags, nil, nil); err != nil {
			return err
		}
		packet := make([]byte, int(frames)*format.blockAlign)
		if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT == 0 && data != nil {
			copy(packet, unsafe.Slice(data, len(packet)))
		}
		var sum float64
		count := 0
		for i := 0; i+1 < len(packet); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(packet[i:]))) / 32768
			sum += v * v
			count++
		}
		level := 0.0
		if count > 0 {
			level = math.Min(1, math.Sqrt(sum/float64(count))*3)
		}
		atomic.StoreUint64(&currentAudioLevel, math.Float64bits(level))
		for _, output := range outputs {
			if output.config.volume >= 0.999 {
				output.queue.push(packet)
				continue
			}
			adjusted := append([]byte(nil), packet...)
			applyPCMVolume(adjusted, output.config.volume, format)
			output.queue.push(adjusted)
		}
		if err := capture.ReleaseBuffer(frames); err != nil {
			return err
		}
	}
}

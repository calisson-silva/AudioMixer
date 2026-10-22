//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/degubites/go-wca/pkg/wca"
	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "AudioMixer"

var errSameEndpoint = errors.New("a interface virtual não pode ser selecionada também como saída física")

// IPolicyConfig is the Windows Core Audio policy interface used to change the
// default render endpoint for the current user. It is part of Windows and does
// not install a helper or require a reboot.
type policyConfig struct{ vtbl *[16]uintptr }

func setDefaultAudioEndpoint(deviceID string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil { return err }
	defer ole.CoUninitialize()
	clsid := ole.NewGUID("{870af99c-171d-4f9e-af0d-e63df40c2bc9}")
	iid := ole.NewGUID("{f8679f50-850a-41cf-9c72-430f290290c8}")
	var policy *policyConfig
	if err := wca.CoCreateInstance(clsid, 0, wca.CLSCTX_ALL, iid, &policy); err != nil { return err }
	defer syscall.Syscall(policy.vtbl[2], 1, uintptr(unsafe.Pointer(policy)), 0, 0)
	id, err := windows.UTF16PtrFromString(deviceID)
	if err != nil { return err }
	for role := uintptr(0); role <= 2; role++ {
		hr, _, _ := syscall.Syscall6(policy.vtbl[13], 4, uintptr(unsafe.Pointer(policy)), uintptr(unsafe.Pointer(id)), role, 0, 0, 0)
		if int32(hr) < 0 { return windows.Errno(hr) }
	}
	return nil
}

type DeviceInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type byteQueue struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func (q *byteQueue) push(data []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(data) >= q.max {
		q.data = append(q.data[:0], data[len(data)-q.max:]...)
		return
	}
	if extra := len(q.data) + len(data) - q.max; extra > 0 {
		copy(q.data, q.data[extra:])
		q.data = q.data[:len(q.data)-extra]
	}
	q.data = append(q.data, data...)
}

func (q *byteQueue) read(dst []byte) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(len(dst), len(q.data))
	copy(dst, q.data[:n])
	copy(q.data, q.data[n:])
	q.data = q.data[:len(q.data)-n]
	return n
}

func enumerateOutputDevices() ([]DeviceInfo, string, error) {
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return nil, "", err
	}
	defer enumerator.Release()

	var defaultID string
	var defaultDevice *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(uint32(wca.ERender), uint32(wca.EMultimedia), &defaultDevice); err == nil {
		_ = defaultDevice.GetId(&defaultID)
		defaultDevice.Release()
	}

	var collection *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(uint32(wca.ERender), wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, defaultID, err
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, defaultID, err
	}
	devices := make([]DeviceInfo, 0, count)
	for i := uint32(0); i < count; i++ {
		var device *wca.IMMDevice
		if err := collection.Item(i, &device); err != nil {
			continue
		}
		info, err := readDeviceInfo(device)
		device.Release()
		if err == nil {
			devices = append(devices, info)
		}
	}
	return devices, defaultID, nil
}

func readDeviceInfo(device *wca.IMMDevice) (DeviceInfo, error) {
	var info DeviceInfo
	if err := device.GetId(&info.ID); err != nil {
		return info, err
	}
	var store *wca.IPropertyStore
	if err := device.OpenPropertyStore(uint32(wca.STGM_READ), &store); err != nil {
		return info, err
	}
	defer store.Release()
	var value wca.PROPVARIANT
	if err := store.GetValue(&wca.PKEY_Device_FriendlyName, &value); err != nil {
		return info, err
	}
	info.Name = value.String()
	value.Clear()
	return info, nil
}

func isRegisteredForStartup() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	value, _, err := key.GetStringValue(runValueName)
	if err != nil {
		return false
	}
	executable, err := os.Executable()
	return err == nil && strings.EqualFold(strings.Trim(value, `"`), executable)
}

func setStartupRegistration(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if !enabled {
		return key.DeleteValue(runValueName)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return key.SetStringValue(runValueName, `"`+executable+`"`)
}

func mirrorAudioSingleLegacy(sourceID, targetID string, additionalDelay time.Duration, stop <-chan struct{}) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		return err
	}
	defer ole.CoUninitialize()

	var enumerator *wca.IMMDeviceEnumerator
	if err = wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return err
	}
	defer enumerator.Release()

	source, err := findDevice(enumerator, sourceID)
	if err != nil {
		return err
	}
	defer source.Release()
	target, err := findDevice(enumerator, targetID)
	if err != nil {
		return err
	}
	defer target.Release()

	var captureClient *wca.IAudioClient
	if err = source.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &captureClient); err != nil {
		return err
	}
	defer captureClient.Release()
	var renderClient *wca.IAudioClient
	if err = target.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &renderClient); err != nil {
		return err
	}
	defer renderClient.Release()

	var format *wca.WAVEFORMATEX
	if err = captureClient.GetMixFormat(&format); err != nil {
		return err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
	if format.NBlockAlign == 0 || format.NAvgBytesPerSec == 0 {
		return errors.New("formato de áudio de origem inválido")
	}

	captureFlags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK | wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	captureFrames, err := initializeSharedStream(captureClient, format, captureFlags)
	if err != nil {
		return fmt.Errorf("captura da saída de origem: %w", err)
	}
	outputFlags := uint32(wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	outputFrames, err := initializeSharedStream(renderClient, format, outputFlags,
		outputFlags|wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM,
		outputFlags|wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM|wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	if err != nil {
		return fmt.Errorf("inicialização da segunda saída: %w", err)
	}
	captureEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("criação do evento de captura: %w", err)
	}
	defer windows.CloseHandle(captureEvent)
	renderEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("criação do evento de reprodução: %w", err)
	}
	defer windows.CloseHandle(renderEvent)
	if err = captureClient.SetEventHandle(uintptr(captureEvent)); err != nil {
		return fmt.Errorf("configuração do evento de captura: %w", err)
	}
	if err = renderClient.SetEventHandle(uintptr(renderEvent)); err != nil {
		return fmt.Errorf("configuração do evento de reprodução: %w", err)
	}

	var capture *wca.IAudioCaptureClient
	if err = captureClient.GetService(wca.IID_IAudioCaptureClient, &capture); err != nil {
		return err
	}
	defer capture.Release()
	var render *wca.IAudioRenderClient
	if err = renderClient.GetService(wca.IID_IAudioRenderClient, &render); err != nil {
		return err
	}
	defer render.Release()

	periodFrames := int(max(captureFrames, outputFrames))
	delayFrames := int(uint64(format.NSamplesPerSec) * uint64(additionalDelay) / uint64(time.Second))
	// Keep two periods as a safety margin, then use a slow adaptive resampler
	// to track small independent-clock drift instead of dropping old audio.
	targetQueueFrames := periodFrames*2 + delayFrames
	queueCapacityFrames := targetQueueFrames + int(format.NSamplesPerSec)/2
	queue := &byteQueue{max: max(queueCapacityFrames*int(format.NBlockAlign), int(format.NBlockAlign))}
	resampleFormat, adaptiveResampling := newPCMFormat(format)
	if !adaptiveResampling {
		logStartup(fmt.Sprintf("adaptive resampling unavailable for PCM format tag=%d bits=%d channels=%d; using packet copy", format.WFormatTag, format.WBitsPerSample, format.NChannels))
	}
	var captureLatency, renderLatency wca.REFERENCE_TIME
	_ = captureClient.GetStreamLatency(&captureLatency)
	_ = renderClient.GetStreamLatency(&renderLatency)
	logStartup(fmt.Sprintf("shared-mode WASAPI: capture=%d frames/%0.2fms render=%d frames/%0.2fms queue-target=%d frames extra-delay=%dms adaptive=%t",
		captureFrames, float64(captureLatency)/10000, outputFrames, float64(renderLatency)/10000, targetQueueFrames, additionalDelay.Milliseconds(), adaptiveResampling))
	if err = captureClient.Start(); err != nil {
		return err
	}
	defer captureClient.Stop()
	// Prime the render FIFO before starting the second endpoint. This avoids
	// beginning with an empty packet and gives the selected delay a stable base.
	for queue.frames(int(format.NBlockAlign)) < targetQueueFrames {
		select {
		case <-stop:
			return nil
		default:
		}
		ready, waitErr := windows.WaitForSingleObject(captureEvent, 50)
		if waitErr != nil {
			return fmt.Errorf("espera pelo buffer inicial de áudio: %w", waitErr)
		}
		if ready == uint32(windows.WAIT_OBJECT_0) {
			if err = copyCapturePackets(capture, queue, int(format.NBlockAlign)); err != nil {
				return err
			}
		}
	}
	if err = renderClient.Start(); err != nil {
		return err
	}
	defer renderClient.Stop()

	events := []windows.Handle{captureEvent, renderEvent}
	resampler := adaptiveResampler{}
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		event, waitErr := windows.WaitForMultipleObjects(events, false, 100)
		if waitErr != nil {
			return fmt.Errorf("espera pelo buffer de áudio: %w", waitErr)
		}
		copyCapture := func() error { return copyCapturePackets(capture, queue, int(format.NBlockAlign)) }
		renderQueued := func() error {
			return renderQueuedAudioSingleLegacy(renderClient, render, queue, outputFrames, int(format.NBlockAlign), resampleFormat, int(format.NSamplesPerSec), targetQueueFrames, &resampler, adaptiveResampling)
		}
		switch event {
		case uint32(windows.WAIT_TIMEOUT):
			continue
		case windows.WAIT_OBJECT_0:
			if err = copyCapture(); err != nil {
				return err
			}
			// WaitForMultipleObjects favours the lower index when both events
			// are signalled. Drain the render event too, so capture cannot starve it.
			if ready, waitErr := windows.WaitForSingleObject(renderEvent, 0); waitErr != nil {
				return waitErr
			} else if ready == uint32(windows.WAIT_OBJECT_0) {
				if err = renderQueued(); err != nil {
					return err
				}
			}
		case windows.WAIT_OBJECT_0 + 1:
			if err = renderQueued(); err != nil {
				return err
			}
			if ready, waitErr := windows.WaitForSingleObject(captureEvent, 0); waitErr != nil {
				return waitErr
			} else if ready == uint32(windows.WAIT_OBJECT_0) {
				if err = copyCapture(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("evento de áudio inesperado: %d", event)
		}
	}
}

// initializeSharedStream uses the device's default shared-mode period. Using
// the minimum period can put unnecessary scheduling pressure on audio drivers;
// shared mode already lets Windows manage the device's real-time scheduling.
func initializeSharedStream(client *wca.IAudioClient, format *wca.WAVEFORMATEX, flagOptions ...uint32) (uint32, error) {
	var client3 *wca.IAudioClient3
	if queryErr := client.PutQueryInterface(wca.IID_IAudioClient3, &client3); queryErr == nil && client3 != nil {
		defer client3.Release()
		var defaultPeriod, fundamentalPeriod, minPeriod, maxPeriod uint32
		if err := client3.GetSharedModeEnginePeriod(format, &defaultPeriod, &fundamentalPeriod, &minPeriod, &maxPeriod); err == nil && defaultPeriod > 0 {
			for _, flags := range flagOptions {
				if err := client3.InitializeSharedAudioStream(flags, defaultPeriod, format, nil); err == nil {
					var frames uint32
					if err := client.GetBufferSize(&frames); err != nil {
						return 0, err
					}
					return frames, nil
				}
			}
		}
	}
	var lastErr error
	for _, flags := range flagOptions {
		if err := client.Initialize(uint32(wca.AUDCLNT_SHAREMODE_SHARED), flags, 0, 0, format, nil); err == nil {
			var frames uint32
			if err := client.GetBufferSize(&frames); err != nil {
				return 0, err
			}
			return frames, nil
		} else {
			lastErr = err
		}
	}
	return 0, lastErr
}

func findDevice(enumerator *wca.IMMDeviceEnumerator, id string) (*wca.IMMDevice, error) {
	var collection *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(uint32(wca.ERender), wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, err
	}
	defer collection.Release()
	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, err
	}
	for i := uint32(0); i < count; i++ {
		var device *wca.IMMDevice
		if err := collection.Item(i, &device); err != nil {
			continue
		}
		var currentID string
		if err := device.GetId(&currentID); err == nil && currentID == id {
			return device, nil
		}
		device.Release()
	}
	return nil, errors.New("a saída selecionada não está mais disponível")
}

func copyCapturePackets(capture *wca.IAudioCaptureClient, queue *byteQueue, blockAlign int) error {
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
		byteCount := int(frames) * blockAlign
		packet := make([]byte, byteCount)
		if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT == 0 && data != nil {
			copy(packet, unsafe.Slice(data, byteCount))
		}
		queue.push(packet)
		if err := capture.ReleaseBuffer(frames); err != nil {
			return err
		}
	}
}

func renderQueuedAudioSingleLegacy(client *wca.IAudioClient, render *wca.IAudioRenderClient, queue *byteQueue, capacity uint32, blockAlign int, format pcmFormat, sampleRate, targetFrames int, resampler *adaptiveResampler, adaptive bool) error {
	var padding uint32
	if err := client.GetCurrentPadding(&padding); err != nil {
		return err
	}
	if padding >= capacity {
		return nil
	}
	frames := capacity - padding
	var data *byte
	if err := render.GetBuffer(frames, &data); err != nil {
		return err
	}
	byteCount := int(frames) * blockAlign
	output := unsafe.Slice(data, byteCount)
	if adaptive {
		queue.renderAdaptive(output, format, sampleRate, targetFrames, resampler)
	} else {
		read := queue.read(output)
		clear(output[read:])
	}
	return render.ReleaseBuffer(frames, 0)
}

func (q *byteQueue) frames(blockAlign int) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if blockAlign <= 0 {
		return 0
	}
	return len(q.data) / blockAlign
}

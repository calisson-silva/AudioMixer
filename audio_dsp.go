//go:build windows

package main

import (
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/degubites/go-wca/pkg/wca"
)

const (
	waveFormatPCM        = 0x0001
	waveFormatIEEEFloat  = 0x0003
	waveFormatExtensible = 0xfffe
)

type pcmFormat struct {
	encoding   uint16
	channels   int
	sampleBits int
	sampleSize int
	blockAlign int
}

func newPCMFormat(format *wca.WAVEFORMATEX) (pcmFormat, bool) {
	result := pcmFormat{
		encoding:   format.WFormatTag,
		channels:   int(format.NChannels),
		sampleBits: int(format.WBitsPerSample),
		sampleSize: int(format.WBitsPerSample) / 8,
		blockAlign: int(format.NBlockAlign),
	}
	if result.encoding == waveFormatExtensible {
		if format.CbSize < 22 {
			return pcmFormat{}, false
		}
		extended := unsafe.Slice((*byte)(unsafe.Pointer(format)), int(unsafe.Sizeof(*format))+int(format.CbSize))
		result.encoding = uint16(binary.LittleEndian.Uint32(extended[24:28]))
	}
	if result.channels < 1 || result.sampleSize < 1 || result.blockAlign < result.channels*result.sampleSize {
		return pcmFormat{}, false
	}
	if result.encoding == waveFormatIEEEFloat {
		return result, result.sampleBits == 32 || result.sampleBits == 64
	}
	if result.encoding == waveFormatPCM {
		switch result.sampleBits {
		case 8, 16, 24, 32:
			return result, true
		}
	}
	return pcmFormat{}, false
}

type adaptiveResampler struct {
	phase float64
	ratio float64
}

func (q *byteQueue) renderAdaptive(dst []byte, format pcmFormat, sampleRate, targetFrames int, resampler *adaptiveResampler) (produced, queued int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	outputFrames := len(dst) / format.blockAlign
	queued = len(q.data) / format.blockAlign
	if outputFrames == 0 || queued < 2 || sampleRate <= 0 {
		clear(dst)
		return 0, queued
	}

	// The FIFO level is the feedback signal for asynchronous sample-rate
	// conversion. Independent audio devices never have perfectly identical
	// sample clocks; this slowly follows their ppm drift without dropping blocks.
	desired := 1 + float64(queued-targetFrames)/float64(sampleRate*5)
	desired = math.Max(0.995, math.Min(1.005, desired))
	if resampler.ratio == 0 {
		resampler.ratio = 1
	}
	resampler.ratio += (desired - resampler.ratio) * 0.08

	maxOutput := int(math.Floor((float64(queued-1)-resampler.phase)/resampler.ratio)) + 1
	produced = min(outputFrames, max(0, maxOutput))
	if produced == 0 {
		clear(dst)
		return 0, queued
	}
	clear(dst)
	for outFrame := 0; outFrame < produced; outFrame++ {
		position := resampler.phase + float64(outFrame)*resampler.ratio
		inFrame := int(position)
		fraction := position - float64(inFrame)
		if inFrame+1 >= queued {
			inFrame = queued - 1
			fraction = 0
		}
		for channel := 0; channel < format.channels; channel++ {
			src0 := inFrame*format.blockAlign + channel*format.sampleSize
			src1 := src0
			if inFrame+1 < queued {
				src1 = (inFrame+1)*format.blockAlign + channel*format.sampleSize
			} else {
				src1 = src0
			}
			a := decodeSample(q.data[src0:src0+format.sampleSize], format)
			b := decodeSample(q.data[src1:src1+format.sampleSize], format)
			value := a + (b-a)*fraction
			dstOffset := outFrame*format.blockAlign + channel*format.sampleSize
			encodeSample(dst[dstOffset:dstOffset+format.sampleSize], value, format)
		}
	}

	consumedPosition := resampler.phase + float64(produced)*resampler.ratio
	consumedFrames := int(consumedPosition)
	if consumedFrames >= queued {
		consumedFrames = queued - 1 // retain one frame for interpolation across packets
	}
	resampler.phase = consumedPosition - float64(consumedFrames)
	if consumedFrames > 0 {
		q.data = q.data[consumedFrames*format.blockAlign:]
	}
	return produced, len(q.data) / format.blockAlign
}

func (q *byteQueue) trimToFrames(frames, blockAlign int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if frames < 0 || blockAlign <= 0 {
		return
	}
	queued := len(q.data) / blockAlign
	if queued > frames {
		drop := queued - frames
		q.data = q.data[drop*blockAlign:]
	}
}

func decodeSample(data []byte, format pcmFormat) float64 {
	switch format.encoding {
	case waveFormatIEEEFloat:
		if format.sampleBits == 32 {
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data))
	case waveFormatPCM:
		switch format.sampleBits {
		case 8:
			return (float64(data[0]) - 128) / 128
		case 16:
			return float64(int16(binary.LittleEndian.Uint16(data))) / 32768
		case 24:
			value := int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16
			if value&0x800000 != 0 {
				value |= ^int32(0xffffff)
			}
			return float64(value) / 8388608
		case 32:
			return float64(int32(binary.LittleEndian.Uint32(data))) / 2147483648
		}
	}
	return 0
}

func encodeSample(data []byte, value float64, format pcmFormat) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		value = 0
	}
	value = math.Max(-1, math.Min(1, value))
	switch format.encoding {
	case waveFormatIEEEFloat:
		if format.sampleBits == 32 {
			binary.LittleEndian.PutUint32(data, math.Float32bits(float32(value)))
		} else {
			binary.LittleEndian.PutUint64(data, math.Float64bits(value))
		}
	case waveFormatPCM:
		switch format.sampleBits {
		case 8:
			data[0] = byte(math.Round(value*127.5 + 127.5))
		case 16:
			sample := int16(math.Round(value * 32767))
			if value <= -1 {
				sample = -32768
			}
			binary.LittleEndian.PutUint16(data, uint16(sample))
		case 24:
			sample := int32(math.Round(value * 8388607))
			if value <= -1 {
				sample = -8388608
			}
			data[0], data[1], data[2] = byte(sample), byte(sample>>8), byte(sample>>16)
		case 32:
			sample := int64(math.Round(value * 2147483647))
			if value <= -1 {
				sample = -2147483648
			}
			binary.LittleEndian.PutUint32(data, uint32(int32(sample)))
		}
	}
}

//go:build windows

package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-ole/go-ole"
)

type audioRun struct {
	stop chan struct{}
	done chan error
}

type DeviceSnapshot struct {
	Devices        []DeviceInfo `json:"devices"`
	DefaultID      string       `json:"defaultId"`
	StartupEnabled bool         `json:"startupEnabled"`
}

type AudioStatus struct {
	Running bool    `json:"running"`
	Message string  `json:"message"`
	Level   float64 `json:"level"`
}

var currentAudioLevel uint64

type AudioService struct {
	mu              sync.Mutex
	run             *audioRun
	status          string
	previousDefault string
	closeToTray     bool
	confirmExit     bool
	minimizeToTray  bool
}

var exitDecision atomic.Int32
var requestQuit func()

func (s *AudioService) SetExitDecision(approved bool) error {
	if approved {
		exitDecision.Store(1)
	} else {
		exitDecision.Store(-1)
	}
	return nil
}
func (s *AudioService) RequestQuit() error {
	if requestQuit != nil {
		requestQuit()
	}
	return nil
}

func (s *AudioService) SetWindowPreferences(minimizeToTray, closeToTray, confirmExit bool) error {
	s.mu.Lock()
	s.minimizeToTray, s.closeToTray, s.confirmExit = minimizeToTray, closeToTray, confirmExit
	s.mu.Unlock()
	return nil
}

func (s *AudioService) MinimizeToTray() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.minimizeToTray
}

func (s *AudioService) WindowPreferences() (closeToTray, confirmExit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeToTray, s.confirmExit
}

func NewAudioService() *AudioService { return &AudioService{status: "Pronto para espelhar o áudio."} }

func (s *AudioService) Devices() (snapshot DeviceSnapshot, err error) {
	devices, defaultID, err := enumerateOutputDevicesCOM()
	if err != nil {
		return DeviceSnapshot{}, err
	}
	return DeviceSnapshot{Devices: devices, DefaultID: defaultID, StartupEnabled: isRegisteredForStartup()}, nil
}

func (s *AudioService) StartMirror(sourceID string, targetIDs []string, delaysMs []int, volumes []float64) error {
	if sourceID == "" || len(targetIDs) == 0 {
		return fmt.Errorf("selecione ao menos uma saída física")
	}
	if len(targetIDs) > 63 {
		return fmt.Errorf("selecione no máximo 63 saídas físicas")
	}
	if len(targetIDs) != len(delaysMs) || len(targetIDs) != len(volumes) {
		return fmt.Errorf("configuração de atraso inválida")
	}
	outputs := make([]audioOutputConfig, 0, len(targetIDs))
	seen := make(map[string]bool, len(targetIDs))
	for i, id := range targetIDs {
		if id == "" || id == sourceID || seen[id] {
			return errSameEndpoint
		}
		if delaysMs[i] < 0 || delaysMs[i] > 1000 {
			return fmt.Errorf("o atraso de cada saída precisa ficar entre 0 e 1000 ms")
		}
		if volumes[i] < 0 || volumes[i] > 1 {
			return fmt.Errorf("volume inválido")
		}
		seen[id] = true
		outputs = append(outputs, audioOutputConfig{deviceID: id, delay: time.Duration(delaysMs[i]) * time.Millisecond, volume: volumes[i]})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil {
		return fmt.Errorf("o espelhamento já está ativo")
	}
	run := &audioRun{stop: make(chan struct{}), done: make(chan error, 1)}
	if s.previousDefault == "" {
		_, previous, _ := enumerateOutputDevicesCOM()
		s.previousDefault = previous
	}
	s.run = run
	s.status = "Iniciando espelhamento..."
	go func() {
		err := mirrorAudio(sourceID, outputs, run.stop)
		run.done <- err
		s.mu.Lock()
		if s.run == run {
			s.run = nil
			if err != nil {
				s.status = "O espelhamento parou: " + err.Error()
			} else {
				s.status = "Espelhamento parado."
			}
		}
		s.mu.Unlock()
	}()
	return nil
}

func (s *AudioService) StopMirror() error {
	s.mu.Lock()
	run := s.run
	if run == nil {
		s.mu.Unlock()
		return nil
	}
	close(run.stop)
	s.status = "Parando espelhamento..."
	s.mu.Unlock()
	err := <-run.done
	s.mu.Lock()
	previous := s.previousDefault
	s.previousDefault = ""
	s.mu.Unlock()
	if previous != "" {
		_ = setDefaultAudioEndpoint(previous)
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *AudioService) Status() AudioStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return AudioStatus{Running: s.run != nil, Message: s.status, Level: math.Float64frombits(atomic.LoadUint64(&currentAudioLevel))}
}

func (s *AudioService) SetStartupEnabled(enabled bool) error {
	if err := setStartupRegistration(enabled); err != nil {
		return err
	}
	return nil
}

// SetDefaultDevice makes the selected virtual render endpoint the Windows
// default for Console, Multimedia and Communications roles.
func (s *AudioService) SetDefaultDevice(deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("dispositivo virtual inválido")
	}
	return setDefaultAudioEndpoint(deviceID)
}

func (s *AudioService) Shutdown() {
	_ = s.StopMirror()
}

func enumerateOutputDevicesCOM() (devices []DeviceInfo, defaultID string, err error) {
	// Each Wails call runs on a Go worker, so COM is initialised on a pinned OS thread.
	// MTA is appropriate for the WASAPI device enumerator.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		return nil, "", err
	}
	defer ole.CoUninitialize()
	return enumerateOutputDevices()
}

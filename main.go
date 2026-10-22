//go:build windows

package main

import (
	"embed"
	"log"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows"
)

//go:embed all:frontend
var frontend embed.FS

//go:embed assets/icons/audiomixer.png
var appIcon []byte

//go:embed assets/icons/tray-light.png
var trayLightIcon []byte

//go:embed assets/icons/tray-dark.png
var trayDarkIcon []byte

func main() {
	logStartup("Wails AudioMixer process started")
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Local\AudioMixer.SingleInstance`))
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		showStartupError("Não foi possível criar o controle de instância única.", err)
		return
	}
	defer windows.CloseHandle(mutex)
	if err == windows.ERROR_ALREADY_EXISTS {
		focusExistingWindow()
		return
	}

	audio := NewAudioService()
	app := application.New(application.Options{
		Name:        "AudioMixer",
		Description: "Espelhamento de áudio para duas saídas",
		Icon:        appIcon,
		Services:    []application.Service{application.NewService(audio)},
		Assets:      application.AssetOptions{Handler: application.BundledAssetFileServer(frontend)},
	})
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "AudioMixer",
		Width:            900,
		Height:           760,
		MinWidth:         900,
		MinHeight:        760,
		DisableResize:    false,
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/",
		Windows:          application.WindowsWindow{BackdropType: application.None},
	})
	// WindowShow is emitted by Wails after the native HWND is actually shown;
	// focusing only immediately after NewWithOptions is too early when launched
	// by the installer or Explorer.
	window.RegisterHook(events.Common.WindowShow, func(_ *application.WindowEvent) {
		go func() {
			time.Sleep(25 * time.Millisecond)
			focusAudioMixerWindow()
		}()
	})

	var exitRequested atomic.Bool
	requestQuit = func() { exitRequested.Store(true); audio.Shutdown(); app.Quit() }
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if exitRequested.Load() {
			return
		}
		closeToTray, confirmExit := audio.WindowPreferences()
		if closeToTray {
			event.Cancel()
			window.Hide()
			return
		}
		if confirmExit && exitDecision.Load() != 1 {
			event.Cancel()
			window.ExecJS("window.showExitDialog && window.showExitDialog();")
			return
		}
		exitDecision.Store(0)
		exitRequested.Store(true)
		audio.Shutdown()
		app.Quit()
	})
	window.OnWindowEvent(events.Common.WindowMinimise, func(_ *application.WindowEvent) {
		if audio.MinimizeToTray() {
			window.Hide()
		}
	})

	tray := app.SystemTray.New()
	trayMenu := app.NewMenu()
	trayMenu.Add("Abrir").OnClick(func(_ *application.Context) {
		window.Show().Focus()
	})
	trayMenu.Add("Sair").OnClick(func(_ *application.Context) {
		_, confirmExit := audio.WindowPreferences()
		if confirmExit && exitDecision.Load() != 1 {
			window.ExecJS("window.showExitDialog && window.showExitDialog();")
			return
		}
		exitDecision.Store(0)
		exitRequested.Store(true)
		audio.Shutdown()
		app.Quit()
	})
	tray.SetIcon(trayLightIcon)
	tray.SetDarkModeIcon(trayDarkIcon)
	tray.SetTooltip("AudioMixer")
	tray.SetMenu(trayMenu)
	tray.OnClick(func() {
		window.Show().Focus()
	})
	tray.OnRightClick(tray.ShowMenu)
	window.Show()
	window.Focus()
	go func() {
		// WebView2 creation can leave the newly launched window behind the
		// installer or the window that launched it. Re-apply activation after
		// the native window exists, then use the Win32 foreground APIs.
		for _, delay := range []time.Duration{150 * time.Millisecond, 450 * time.Millisecond, 900 * time.Millisecond} {
			time.Sleep(delay)
			window.Show()
			window.Focus()
			focusAudioMixerWindow()
		}
	}()

	if err := app.Run(); err != nil {
		logStartup("Wails run failed: " + err.Error())
		log.Fatal(err)
	}
	audio.Shutdown()
	time.Sleep(50 * time.Millisecond)
}

func focusAudioMixerWindow() {
	title, _ := windows.UTF16PtrFromString("AudioMixer")
	user32 := windows.NewLazySystemDLL("user32.dll")
	findWindow := user32.NewProc("FindWindowW")
	showWindow := user32.NewProc("ShowWindow")
	bringToTop := user32.NewProc("BringWindowToTop")
	setForeground := user32.NewProc("SetForegroundWindow")
	setActive := user32.NewProc("SetActiveWindow")
	switchTo := user32.NewProc("SwitchToThisWindow")
	allowForeground := user32.NewProc("AllowSetForegroundWindow")
	getForeground := user32.NewProc("GetForegroundWindow")
	getWindowThread := user32.NewProc("GetWindowThreadProcessId")
	getCurrentThread := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentThreadId")
	attachThread := user32.NewProc("AttachThreadInput")
	setWindowPos := user32.NewProc("SetWindowPos")
	hwnd, _, _ := findWindow.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	const swRestore = 9
	const hwndTopmost = ^uintptr(0)
	const hwndNoTopmost = ^uintptr(1)
	const swpNoSize = 0x0001
	const swpNomove = 0x0002
	const swpNoActivate = 0x0010
	showWindow.Call(hwnd, swRestore)
	// Windows normally denies foreground activation when another process has
	// focus (for example Explorer after an MSI finishes). Temporarily attach
	// the input queues so this process is allowed to activate its own window.
	allowForeground.Call(^uintptr(0)) // ASFW_ANY
	var foregroundPID uint32
	foreground, _, _ := getForeground.Call()
	windowThread, _, _ := getWindowThread.Call(hwnd, uintptr(unsafe.Pointer(&foregroundPID)))
	foregroundThread, _, _ := getWindowThread.Call(foreground, uintptr(unsafe.Pointer(&foregroundPID)))
	currentThread, _, _ := getCurrentThread.Call()
	if foregroundThread != 0 && currentThread != foregroundThread {
		attachThread.Call(currentThread, foregroundThread, 1)
		defer attachThread.Call(currentThread, foregroundThread, 0)
	}
	if windowThread != 0 && currentThread != windowThread {
		attachThread.Call(currentThread, windowThread, 1)
		defer attachThread.Call(currentThread, windowThread, 0)
	}
	setActive.Call(hwnd)
	bringToTop.Call(hwnd)
	switchTo.Call(hwnd, 1)
	setForeground.Call(hwnd)
	// Toggle topmost without leaving the app permanently topmost. This helps
	// Windows honor activation when the MSI or another app currently owns focus.
	setWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNomove|swpNoSize|swpNoActivate)
	setWindowPos.Call(hwnd, hwndNoTopmost, 0, 0, 0, 0, swpNomove|swpNoSize|swpNoActivate)
}

func focusExistingWindow() {
	title, _ := windows.UTF16PtrFromString("AudioMixer")
	user32 := windows.NewLazySystemDLL("user32.dll")
	findWindow := user32.NewProc("FindWindowW")
	showWindow := user32.NewProc("ShowWindow")
	setForeground := user32.NewProc("SetForegroundWindow")
	for i := 0; i < 40; i++ {
		hwnd, _, _ := findWindow.Call(0, uintptr(unsafe.Pointer(title)))
		if hwnd != 0 {
			_, _, _ = showWindow.Call(hwnd, 9) // SW_RESTORE
			_, _, _ = setForeground.Call(hwnd)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	showStartupError("O AudioMixer já está aberto.", nil)
}

func showStartupError(message string, cause error) {
	if cause != nil {
		message += "\n\n" + cause.Error()
	}
	title, _ := windows.UTF16PtrFromString("AudioMixer")
	text, _ := windows.UTF16PtrFromString(message)
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
}

func logStartup(message string) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return
	}
	dir := base + `\AudioMixer`
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	f, err := os.OpenFile(dir+`\startup.log`, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format(time.RFC3339) + " " + message + "\r\n")
}

SHELL := cmd.exe
.SHELLFLAGS := /d /c

.PHONY: build clean

build:
	call build.bat

clean:
	if exist dist rmdir /s /q dist

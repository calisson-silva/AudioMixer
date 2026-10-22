# AudioMixer

AudioMixer é um aplicativo Windows para espelhar em tempo real o áudio reproduzido em uma saída para duas ou mais saídas físicas.

## Recursos

- Interface em português usando Wails v3, HTML, CSS e JavaScript.
- Captura por WASAPI loopback, sem driver virtual e sem acesso a microfone.
- Seleção manual da origem e das saídas físicas.
- Atraso individual de 0 a 1000 ms e volume independente por saída.
- Monitor LED em tempo real e atualização dos dispositivos sem reiniciar.
- Inicialização com o Windows, systray, instância única e confirmação de saída.

O aplicativo usa WASAPI em modo compartilhado e não requer privilégios administrativos para executar.

## Compilação no Windows

Requisitos: Windows x64, Go 1.27.2 ou superior e Microsoft Edge WebView2 Runtime.

```bat
build.bat
```

O executável será criado em `dist\\AudioMixer.exe`. A compilação usa o Go nativo do Windows e não precisa de WSL, Node.js ou CLI do Wails.

## Estrutura

- `main.go`, `service.go`, `audio.go`: janela, ciclo de vida e roteamento WASAPI.
- `audio_dsp.go`, `audio_multiroute.go`: processamento e múltiplas saídas.
- `frontend/`: interface, estilos, monitor LED e bindings Wails.
- `assets/`: ícones e recursos visuais.
- `tools/`: ferramentas de desenvolvimento.
- `build.bat`, `Makefile`: compilação nativa.

## Diagnóstico

O log fica em `%LOCALAPPDATA%\\AudioMixer\\startup.log`.

## Créditos

Calisson Silva  
calisson87@gmail.com

## Licença

Este repositório ainda não define uma licença de redistribuição.

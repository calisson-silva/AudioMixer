import { AudioService } from "./bindings/audiomirror/index.js";
window.showExitDialog = () => document.querySelector("#exitDialog")?.showModal();
document.querySelector("#cancelExit")?.addEventListener("click", () => document.querySelector("#exitDialog")?.close("cancel"));
document.querySelector("#confirmExitButton")?.addEventListener("click", async () => { await AudioService.SetExitDecision(true); document.querySelector("#exitDialog")?.close("confirm"); await AudioService.RequestQuit(); });

const sourceName = document.querySelector("#sourceSelect");
const outputs = document.querySelector("#outputs");
const startButton = document.querySelector("#startButton");
const startup = document.querySelector("#startup");
const statusDot = document.querySelector("#statusDot");
const statusTitle = document.querySelector("#statusTitle");
const statusMessage = document.querySelector("#statusMessage");
const refreshButton = document.querySelector("#refresh");
const waveCanvas = document.querySelector("#audioWave");
const waveContext = waveCanvas?.getContext("2d");
let audioMotion;
let monitorGain;
let monitorContextRef;
try {
  const monitorContext = new AudioContext();
  monitorContextRef = monitorContext;
  const monitorOscillator = monitorContext.createOscillator();
  monitorGain = monitorContext.createGain();
  monitorGain.gain.value = 0;
  monitorOscillator.frequency.value = 110;
  const Analyzer = window.AudioMotionAnalyzer;
  if (!Analyzer) throw new Error("audioMotion indisponível");
  audioMotion = new Analyzer({ canvas: waveCanvas, audioCtx: monitorContext, connectSpeakers: false, ledBars: true, mode: 0, barSpace: 0.35, gradient: "prism", showScaleX: false, showScaleY: false });
  audioMotion.connectInput(monitorGain);
  audioMotion.start();
  monitorOscillator.start();
} catch { audioMotion = null; }
const themeColor = document.querySelector("#themeColor");
if (themeColor) {
  const savedTheme = localStorage.getItem("themeColor") || "#4b8df8";
  themeColor.value = savedTheme;
  document.documentElement.style.setProperty("--blue", savedTheme);
  themeColor.addEventListener("input", () => { document.documentElement.style.setProperty("--blue", themeColor.value); localStorage.setItem("themeColor", themeColor.value); });
}
async function saveWindowPreferences() {
  const closeToTray = document.querySelector("#closeTray")?.checked ?? false;
  const minimizeToTray = document.querySelector("#minimizeTray")?.checked ?? false;
  const confirmExit = document.querySelector("#confirmExit")?.checked ?? false;
  try { await AudioService.SetWindowPreferences(minimizeToTray, closeToTray, confirmExit); } catch (error) { console.error(error); }
}
for (const id of ["minimizeTray", "closeTray", "confirmExit"]) {
  const control = document.querySelector(`#${id}`);
  if (control) { control.checked = localStorage.getItem(`setting:${id}`) === "1"; control.addEventListener("change", () => { localStorage.setItem(`setting:${id}`, control.checked ? "1" : "0"); saveWindowPreferences(); }); }
}
saveWindowPreferences();
const setDefaultButton = document.querySelector("#setDefault"); if (setDefaultButton) setDefaultButton.hidden = true;
let devices = [];
let busy = false;
let defaultId = "";
let sourceId = "";
let virtualSourceReady = false;
let virtualDeviceInstalled = false;
let virtualDeviceName = "AudioMixer";
let deviceSignature = "";
let liveVolumeTimer = 0;
let wavePhase = 0;
let audioLevel = 0;
function drawWave() {
  if (!waveContext || !waveCanvas) return;
  const w = waveCanvas.width, h = waveCanvas.height;
  waveContext.clearRect(0, 0, w, h);
  const bars = 32, gap = 4, barWidth = Math.max(2, (w - gap * (bars - 1)) / bars);
  for (let i = 0; i < bars; i++) {
    const idle = 0.06 + Math.abs(Math.sin(i * .8)) * .035;
    const pulse = busy ? audioLevel * (0.55 + Math.abs(Math.sin(i * .47 + wavePhase)) * .45) : idle;
    const level = Math.min(1, pulse);
    const segments = 24, segGap = 2, segHeight = (h - (segments - 1) * segGap) / segments;
    for (let s = 0; s < segments; s++) {
      const threshold = (s + 1) / segments;
      const y = h - (s + 1) * (segHeight + segGap) + segGap;
      const lit = threshold <= level;
      const hue = s >= 21 ? "#ff3030" : s >= 18 ? "#ff7518" : s >= 14 ? "#ffd21c" : "#55f21b";
      waveContext.fillStyle = lit ? hue : "#252d3a";
      waveContext.fillRect(i * (barWidth + gap), y, barWidth, segHeight);
    }
  }
  wavePhase += busy ? .12 : .02; requestAnimationFrame(drawWave);
}
drawWave();
const selectedKey = "mirrorTargetIds";
const delaysKey = "mirrorTargetDelays";
const volumesKey = "mirrorTargetVolumes";
const sourceKey = "mirrorSourceId";
const virtualRenderPattern = /speakers \(vb-audio virtual cable\)|cable in 16 ?ch|cable input/i;
const virtualDevicePattern = /cable output|cable input|cable in 16 ?ch|speakers \(vb-audio virtual cable\)|voicemeeter input|audiomixer virtual/i;
function savedDelays() {
  try { return JSON.parse(localStorage.getItem(delaysKey) ?? "{}"); } catch { return {}; }
}
function savedVolumes() { try { return JSON.parse(localStorage.getItem(volumesKey) ?? "{}"); } catch { return {}; } }
function defaultDelay(device) {
  const name = device.name.toLowerCase();
  return /hdmi|televis|\btv\b|displayport/.test(name) ? 0 : 80;
}
function selectedTargets() {
  return [...outputs.querySelectorAll("input[data-output]:checked")];
}
function currentMirrorConfig() {
  const selected = selectedTargets();
  return {
    ids: selected.map(input => input.dataset.output),
    delays: selected.map(input => Number(input.closest(".output-row").querySelector(".output-sync input").value)),
    volumes: selected.map(input => Number(input.closest(".output-row").querySelector(".volume-slider").value) / 100),
  };
}
function scheduleLiveVolumeUpdate() {
  if (!busy) return;
  window.clearTimeout(liveVolumeTimer);
  liveVolumeTimer = window.setTimeout(async () => {
    try {
      const config = currentMirrorConfig();
      await AudioService.StopMirror();
      await AudioService.StartMirror(sourceId || sourceName.value, config.ids, config.delays, config.volumes);
      await updateStatus();
    } catch (error) { setStatus("ERRO AO ATUALIZAR VOLUME", String(error), "error"); }
  }, 120);
}

function setStatus(title, message, kind = "ready") {
  statusTitle.textContent = title;
  statusMessage.textContent = message;
  statusDot.className = `status-indicator ${kind === "ready" ? "" : kind}`;
}
function fillOutputs() {
  outputs.replaceChildren();
  const available = devices.filter(device => device.id !== (sourceId || sourceName.value));
  let selected;
  try { selected = new Set(JSON.parse(localStorage.getItem(selectedKey) ?? "[]")); } catch { selected = new Set(); }
  const hasSavedSelection = localStorage.getItem(selectedKey) !== null;
  const delays = savedDelays();
  const volumes = savedVolumes();
  for (const device of available) {
    const row = document.createElement("div");
    row.className = "output-row";
    const label = document.createElement("label");
    label.className = "output-choice";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.dataset.output = device.id;
    checkbox.checked = hasSavedSelection ? selected.has(device.id) : true;
    checkbox.addEventListener("change", () => {
      localStorage.setItem(selectedKey, JSON.stringify(selectedTargets().map(input => input.dataset.output)));
    });
    const name = document.createElement("span");
    name.className = "output-name";
    name.textContent = device.name;
    label.append(checkbox, name);
    const sync = document.createElement("label");
    sync.className = "output-sync";
    const range = document.createElement("input");
    range.type = "number";
    range.min = "0";
    range.max = "1000";
    range.step = "1";
    range.value = String(delays[device.id] ?? defaultDelay(device));
    range.setAttribute("aria-label", `Atraso para ${device.name}`);
    const value = document.createElement("output");
    value.textContent = "ms";
    range.addEventListener("input", () => {
      range.value = String(Math.max(0, Math.min(1000, Number(range.value) || 0)));
      value.textContent = "ms";
      const current = savedDelays();
      current[device.id] = Number(range.value);
      localStorage.setItem(delaysKey, JSON.stringify(current));
    });
    range.addEventListener("change", scheduleLiveVolumeUpdate);
    sync.append(range, value);
    const volumeButton = document.createElement("button");
    volumeButton.type = "button"; volumeButton.className = "volume-button"; volumeButton.textContent = "🔊";
    const volume = document.createElement("input"); volume.type = "range"; volume.min="0"; volume.max="100"; volume.value=String(Math.round((volumes[device.id] ?? 1)*100)); volume.className="volume-slider";
    volume.setAttribute("aria-label", `Volume para ${device.name}`);
    volumeButton.addEventListener("click", () => volume.focus());
    volume.addEventListener("input", () => { const v=savedVolumes(); v[device.id]=Number(volume.value)/100; localStorage.setItem(volumesKey, JSON.stringify(v)); volumeButton.textContent = Number(volume.value) === 0 ? "🔇" : "🔊"; });
    volume.addEventListener("change", scheduleLiveVolumeUpdate);
    const separator = document.createElement("span");
    separator.className = "control-separator";
    sync.append(separator, volumeButton, volume);
    row.append(label, sync);
    outputs.append(row);
  }
  if (!available.length) {
    const empty = document.createElement("div");
    empty.className = "empty-output";
    empty.textContent = "Nenhuma saída disponível além da origem selecionada.";
    outputs.append(empty);
  }
}
async function loadDevices() {
  refreshButton.disabled = true;
  try {
    const snapshot = await AudioService.Devices();
    devices = snapshot.devices ?? [];
    defaultId = snapshot.defaultId || "";
    const previousSource = sourceId || localStorage.getItem(sourceKey) || "";
    const selectedStillExists = devices.some(device => device.id === previousSource);
    const nextSource = selectedStillExists ? previousSource : defaultId;
    sourceName.replaceChildren(...devices.map(device => { const option=document.createElement("option"); option.value=device.id; option.textContent=device.name; return option; }));
    sourceName.value = nextSource;
    sourceId = nextSource;
    if (sourceId) localStorage.setItem(sourceKey, sourceId);
    virtualDeviceInstalled = devices.some(device => virtualDevicePattern.test(device.name));
    virtualDeviceName = "AudioMixer";
    virtualSourceReady = Boolean(devices.some(device => device.id === sourceId));
    fillOutputs();
    startup.checked = snapshot.startupEnabled;
    setStatus(virtualSourceReady ? "PRONTO PARA ROTEAR" : "SELECIONE UMA ORIGEM",
      virtualSourceReady ? "Marque as saídas que devem tocar." : "Escolha um dispositivo de origem para iniciar.",
      virtualSourceReady ? "ready" : "error");
  } catch (error) {
    setStatus("ERRO", String(error), "error");
  } finally { refreshButton.disabled = false; }
}
async function updateStatus() {
  try {
    const state = await AudioService.Status();
    busy = state.running;
    if (state.running && monitorContextRef?.state === "suspended") monitorContextRef.resume().catch(() => {});
    audioLevel = Math.max(0, Math.min(1, Number(state.level) || 0));
    if (monitorGain) monitorGain.gain.value = state.running ? audioLevel * 0.08 : 0;
    startButton.querySelector("span:last-child").textContent = state.running ? "Parar espelhamento" : "Iniciar espelhamento";
    startButton.querySelector(".play-icon").textContent = state.running ? "■" : "▶";
    startButton.disabled = !state.running && (!virtualSourceReady || selectedTargets().length === 0);
    if (!state.running && !virtualSourceReady) {
      setStatus("SELECIONE UMA ORIGEM", "Escolha um dispositivo de origem para iniciar.", "error");
    } else {
      setStatus(state.running ? "ROTEANDO" : (state.message.includes("parou") ? "INTERROMPIDO" : "PRONTO"),
        state.message, state.running ? "busy" : "ready");
    }
  } catch { }
}
startButton.addEventListener("click", async () => {
  startButton.disabled = true;
  try {
    if (busy) await AudioService.StopMirror();
    else {
      const selected = selectedTargets();
      const delays = selected.map(input => Number(input.closest(".output-row").querySelector(".output-sync input").value));
      const volumes = selected.map(input => Number(input.closest(".output-row").querySelector(".volume-slider").value)/100);
      await AudioService.StartMirror(sourceId || sourceName.value, selected.map(input => input.dataset.output), delays, volumes);
    }
    await updateStatus();
  } catch (error) { setStatus("NÃO FOI POSSÍVEL INICIAR", String(error), "error"); }
  finally { startButton.disabled = false; }
});
startup.addEventListener("change", async () => {
  startup.disabled = true;
  try { await AudioService.SetStartupEnabled(startup.checked); setStatus("PREFERÊNCIA SALVA", startup.checked ? "O AudioMixer iniciará com o Windows." : "Inicialização automática desativada."); }
  catch (error) { startup.checked = !startup.checked; setStatus("ERRO AO SALVAR", String(error), "error"); }
  finally { startup.disabled = false; }
});
refreshButton.addEventListener("click", loadDevices);
sourceName.addEventListener("change", () => { sourceId = sourceName.value; localStorage.setItem(sourceKey, sourceId); fillOutputs(); updateStatus(); });
setDefaultButton?.addEventListener("click", async () => {
  try {
    const virtual = devices.find(device => virtualRenderPattern.test(device.name));
    if (!virtual) throw new Error("dispositivo virtual AudioMixer não encontrado");
    await AudioService.SetDefaultDevice(virtual.id);
    await loadDevices();
  }
  catch (error) { setStatus("ERRO", String(error), "error"); }
});
loadDevices();
setInterval(updateStatus, 700);
setInterval(async () => {
  try {
    const snapshot = await AudioService.Devices();
    const signature = JSON.stringify((snapshot.devices ?? []).map(d => `${d.id}:${d.name}`).sort());
    if (deviceSignature && signature !== deviceSignature) await loadDevices();
    deviceSignature = signature;
  } catch { }
}, 1200);





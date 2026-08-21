'use strict';

const state = {
  apps: [], devices: [], deviceGroups: [], drivers: [], scenes: [], flows: [], flowCards: { triggers: [], conditions: [], actions: [] },
  // De schakelaars van Stulp zelf. Hij staat als eerste in de app-lijst.
  system: {},
  collapsedGroups: new Set(), editingGroup: null, openDeviceID: null, deviceTab: 'overview',
  devicePress: null, deviceOrder: null, suppressDeviceClickUntil: 0,
  editingScene: null, editingFlow: null, flowPickerKind: null, flowPickerDevice: null, flowMove: null, flowLink: null, flowAddPoint: null,
  pair: null, peer: null, settingsAppId: null,
};
const $ = id => document.getElementById(id);
const nativeSceneAppID = 'com.stulp.scene';
let autocompleteID = 0;
let flowDrawFrame = 0;
let realtimeReloadTimer = 0;
let realtimeAbort = new AbortController();
let loadPromise = null;
let loadRequested = false;
let realtimeDeviceUpdatesDuringLoad = null;
const flowResizeObserver = new ResizeObserver(() => scheduleFlowConnections());
const deviceLongPressDelay = 450;
const devicePressMoveTolerance = 10;

async function api(path, options = {}) {
  const headers = new Headers(options.headers);
  if (options.body) headers.set('Content-Type', 'application/json');
  const response = await fetch(path, { ...options, headers });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.error_description || body?.error || `HTTP ${response.status}`);
  return body;
}
function node(tag, className, text) {
  const result = document.createElement(tag);
  if (className) result.className = className;
  if (text !== undefined) result.textContent = text;
  return result;
}
function materialIcon(name, className = '') {
  const icon = node('span', `material-symbols-rounded ${className}`.trim(), name);
  icon.setAttribute('aria-hidden', 'true');
  return icon;
}
function localized(value) {
  if (typeof value === 'string') return value;
  return value?.nl || value?.en || Object.values(value || {})[0] || '';
}
function encode(value) { return encodeURIComponent(value); }

// Reloads are snapshots assembled from several endpoints. Keep only one in
// flight: two snapshots that finish out of order can otherwise put yesterday's
// device state back after a newer one.
function load() {
  loadRequested = true;
  if (loadPromise) return loadPromise;
  loadPromise = (async () => {
    try {
      while (loadRequested) {
        loadRequested = false;
        await loadSnapshot();
      }
    } finally {
      loadPromise = null;
    }
  })();
  return loadPromise;
}

function deviceWithLocalUIState(device, previous) {
  return previous
    ? { ...device, media: previous.media, mediaLoaded: previous.mediaLoaded, mediaLoading: false }
    : device;
}

async function loadSnapshot() {
  // Device updates keep arriving over SSE while the other snapshot requests
  // (notably Flow registrations) are still pending. Remember their newest
  // full snapshot per device, so the older GET /devices answer cannot erase a
  // value or availability bit that was already shown.
  const liveDeviceUpdates = new Map();
  realtimeDeviceUpdatesDuringLoad = liveDeviceUpdates;
  try {
    const previousDevices = new Map(state.devices.map(device => [device.id, device]));
    const [health, apps, devices, deviceGroups, drivers, scenes, flows, flowCards, system] = await Promise.all([
      api('/api/stulp/health'), api('/api/manager/apps/app'),
      api('/api/manager/devices/device'), api('/api/stulp/device-groups'), api('/api/manager/drivers/driver'),
      api('/api/stulp/scenes'), api('/api/manager/flow/flow'), api('/api/stulp/flow/cards'), api('/api/stulp/system'),
    ]);
    state.system = { ...system, version: health.stulpVersion || health.version || '' };
    state.apps = Object.values(apps);
    const currentDevices = new Map(state.devices.map(device => [device.id, device]));
    const loadedDevices = Object.values(devices).map(device => deviceWithLocalUIState(
      device, currentDevices.get(device.id) || previousDevices.get(device.id),
    ));
    for (const updated of liveDeviceUpdates.values()) {
      const merged = deviceWithLocalUIState(updated, currentDevices.get(updated.id) || previousDevices.get(updated.id));
      const index = loadedDevices.findIndex(device => device.id === updated.id);
      if (index < 0) loadedDevices.push(merged);
      else loadedDevices[index] = merged;
    }
    state.devices = loadedDevices;
    state.deviceGroups = Array.isArray(deviceGroups) ? deviceGroups : Object.values(deviceGroups);
    state.drivers = Object.values(drivers);
    state.scenes = Array.isArray(scenes) ? scenes : Object.values(scenes);
    state.flows = Array.isArray(flows) ? flows : Object.values(flows);
    state.flowCards = flowCards;
    $('connection').textContent = health.ok ? 'Online' : 'Offline';
    renderDevices();
    renderApps();
    renderFlows();
  } catch (error) {
    $('connection').textContent = 'Offline';
    toast(error.message, true);
  } finally {
    if (realtimeDeviceUpdatesDuringLoad === liveDeviceUpdates) realtimeDeviceUpdatesDuringLoad = null;
  }
}

function renderDevices() {
  if (state.deviceOrder) return;
  const list = $('devices');
  list.replaceChildren();
  if (!state.devices.length && !state.deviceGroups.length) return list.append(node('p', 'empty', 'Nog geen devices.'));
  rootDeviceGroups().forEach(group => list.append(renderDeviceGroup(group)));
  const ungrouped = state.devices
    .filter(device => !device.groupId || !state.deviceGroups.some(group => group.id === device.groupId))
    .sort(compareDevices);
  if (ungrouped.length) list.append(renderDeviceGroup({ id: '', name: 'Overig', parentId: '', devices: ungrouped }));
}

function compareGroups(left, right) {
  return Number(left.sortOrder || 0) - Number(right.sortOrder || 0) || left.name.localeCompare(right.name, 'nl') || left.id.localeCompare(right.id);
}

function rootDeviceGroups() {
  const ids = new Set(state.deviceGroups.map(group => group.id));
  return state.deviceGroups
    .filter(group => !group.parentId || !ids.has(group.parentId))
    .sort(compareGroups);
}

function deviceGroupsInDisplayOrder() {
  const result = [];
  const append = group => {
    result.push(group);
    childGroups(group.id).forEach(append);
  };
  rootDeviceGroups().forEach(append);
  return result;
}

function childGroups(parentID) {
  return state.deviceGroups.filter(group => (group.parentId || '') === (parentID || '')).sort(compareGroups);
}

function directGroupDevices(groupID) {
  return state.devices.filter(device => device.groupId === groupID).sort(compareDevices);
}

function compareDevices(left, right) {
  const leftOrder = Math.max(0, Number(left.sortOrder) || 0);
  const rightOrder = Math.max(0, Number(right.sortOrder) || 0);
  if (leftOrder > 0 && rightOrder > 0 && leftOrder !== rightOrder) return leftOrder - rightOrder;
  return left.name.localeCompare(right.name, 'nl') || left.id.localeCompare(right.id);
}

function groupDeviceCount(groupID) {
  return directGroupDevices(groupID).length + childGroups(groupID).reduce((total, child) => total + groupDeviceCount(child.id), 0);
}

function groupPath(groupID) {
  const names = [];
  const seen = new Set();
  for (let group = state.deviceGroups.find(candidate => candidate.id === groupID); group && !seen.has(group.id); group = state.deviceGroups.find(candidate => candidate.id === group.parentId)) {
    seen.add(group.id); names.unshift(group.name);
  }
  return names.join(' › ');
}

function descendantGroupIDs(groupID) {
  const result = new Set(groupID ? [groupID] : []);
  for (const id of result) childGroups(id).forEach(group => result.add(group.id));
  return result;
}

function appendGroupOptions(select, selectedID = '', excludedID = '') {
  const excluded = descendantGroupIDs(excludedID);
  const ids = new Set(state.deviceGroups.map(group => group.id));
  const append = (group, depth) => {
    if (excluded.has(group.id)) return;
    const option = node('option', '', `${'— '.repeat(depth)}${group.name}`);
    option.value = group.id; option.selected = group.id === selectedID; select.append(option);
    childGroups(group.id).forEach(child => append(child, depth + 1));
  };
  state.deviceGroups.filter(group => !group.parentId || !ids.has(group.parentId)).sort(compareGroups).forEach(group => append(group, 0));
}

function renderDeviceGroup(group) {
  const children = group.id ? childGroups(group.id) : [];
  const directDevices = group.devices || directGroupDevices(group.id);
  const section = node('section', 'device-group');
  section.dataset.groupId = group.id;
  section.dataset.parentId = group.parentId || '';
  const heading = node('div', 'device-group-heading');
  const toggle = node('button', 'device-group-toggle');
  toggle.type = 'button';
  const collapsed = state.collapsedGroups.has(group.id || '__other');
  toggle.setAttribute('aria-expanded', String(!collapsed));
  toggle.append(materialIcon(collapsed ? 'chevron_right' : 'expand_more', 'group-chevron'), node('strong', '', group.name), node('span', 'group-count', String(group.id ? groupDeviceCount(group.id) : directDevices.length)));
  toggle.addEventListener('click', () => {
    const key = group.id || '__other';
    state.collapsedGroups.has(key) ? state.collapsedGroups.delete(key) : state.collapsedGroups.add(key);
    renderDevices();
  });
  heading.append(toggle);
  if (group.id) {
    const groupActions = node('div', 'device-group-actions');
    const siblings = childGroups(group.parentId || '');
    const index = siblings.findIndex(candidate => candidate.id === group.id);
    const up = compactIconButton('arrow_upward', 'Groep omhoog', () => moveDeviceGroupOrder(group, -1)); up.disabled = index <= 0;
    const down = compactIconButton('arrow_downward', 'Groep omlaag', () => moveDeviceGroupOrder(group, 1)); down.disabled = index < 0 || index >= siblings.length - 1;
    groupActions.append(up, down, compactIconButton('more_horiz', 'Groep bewerken', () => openGroupEditor(group)));
    heading.append(groupActions);
  }
  section.append(heading);
  const contents = node('div', `device-group-contents ${collapsed ? 'hidden' : ''}`);
  if (directDevices.length) {
    const devices = node('div', 'device-list');
    directDevices.forEach(device => devices.append(renderDevice(device)));
    contents.append(devices);
  }
  if (children.length) {
    const nested = node('div', 'device-group-children');
    children.forEach(child => nested.append(renderDeviceGroup(child)));
    contents.append(nested);
  }
  if (!directDevices.length && !children.length) contents.append(node('p', 'empty group-empty', 'Deze groep is nog leeg.'));
  section.append(contents);
  return section;
}

function renderDevice(device) {
  const card = node('article', `device-card ${device.available ? '' : 'unavailable'}`);
  card.dataset.deviceId = device.id;
  const summary = node('button', 'device-summary');
  summary.type = 'button';
  summary.setAttribute('aria-label', `${device.name} openen`);
  summary.title = `${device.name} openen · houd ingedrukt om te ordenen`;
  summary.append(deviceClassIcon(device.class), node('strong', 'device-name', device.name));
  summary.addEventListener('pointerdown', event => armDeviceOrder(event, device, card, summary));
  summary.addEventListener('contextmenu', event => {
    if (state.devicePress || state.deviceOrder) event.preventDefault();
  });
  summary.addEventListener('click', event => {
    if (Date.now() < state.suppressDeviceClickUntil) {
      event.preventDefault(); event.stopPropagation();
      return;
    }
    prepareDevicePopover(device);
  });
  card.append(summary);
  const quick = deviceQuickControl(device);
  if (quick) card.append(quick);
  return card;
}

function armDeviceOrder(event, device, card, target) {
  if (!event.isPrimary || event.button !== 0 || state.deviceOrder) return;
  cancelDevicePress();
  const press = {
    pointerId: event.pointerId, device, card, target,
    startX: event.clientX, startY: event.clientY, lastX: event.clientX, lastY: event.clientY, timer: 0,
  };
  state.devicePress = press;
  card.classList.add('sort-pressing');
  press.timer = setTimeout(() => beginDeviceOrder(press), deviceLongPressDelay);
}

function cancelDevicePress(pointerId = null) {
  const press = state.devicePress;
  if (!press || pointerId !== null && press.pointerId !== pointerId) return;
  clearTimeout(press.timer);
  press.card.classList.remove('sort-pressing');
  state.devicePress = null;
}

function beginDeviceOrder(press) {
  if (state.devicePress !== press) return;
  clearTimeout(press.timer);
  state.devicePress = null;
  const list = press.card.parentElement;
  const group = press.card.closest('.device-group');
  if (!list?.classList.contains('device-list') || !group) return;
  const bounds = press.card.getBoundingClientRect();
  const ghost = press.card.cloneNode(true);
  ghost.classList.remove('sort-pressing');
  ghost.classList.add('device-sort-ghost');
  ghost.removeAttribute('data-device-id');
  ghost.setAttribute('aria-hidden', 'true');
  ghost.style.width = `${bounds.width}px`;
  ghost.style.height = `${bounds.height}px`;
  document.body.append(ghost);
  press.card.classList.remove('sort-pressing');
  press.card.classList.add('sorting-source');
  list.classList.add('sorting');
  document.body.classList.add('device-ordering');
  $('device-order-hint').classList.remove('hidden');
  state.deviceOrder = {
    pointerId: press.pointerId, card: press.card, list, ghost,
    groupID: group.dataset.groupId || '',
    originalIDs: [...list.querySelectorAll(':scope > .device-card')].map(card => card.dataset.deviceId),
  };
  moveDeviceOrderGhost(press.lastX, press.lastY);
}

function moveDeviceOrderGhost(x, y) {
  const order = state.deviceOrder;
  if (!order) return;
  order.ghost.style.left = `${x}px`;
  order.ghost.style.top = `${y}px`;
}

function moveDeviceOrder(event) {
  const press = state.devicePress;
  if (press && event.pointerId === press.pointerId) {
    press.lastX = event.clientX; press.lastY = event.clientY;
    if (Math.hypot(event.clientX - press.startX, event.clientY - press.startY) > devicePressMoveTolerance) cancelDevicePress(event.pointerId);
  }
  const order = state.deviceOrder;
  if (!order || event.pointerId !== order.pointerId) return;
  event.preventDefault();
  moveDeviceOrderGhost(event.clientX, event.clientY);
  const target = document.elementFromPoint(event.clientX, event.clientY)?.closest('.device-card');
  if (!target || target === order.card || target.parentElement !== order.list || target.classList.contains('device-sort-ghost')) return;
  const bounds = target.getBoundingClientRect();
  const middleX = bounds.left + bounds.width / 2;
  const middleY = bounds.top + bounds.height / 2;
  const before = event.clientY < middleY - bounds.height * .2 ||
    Math.abs(event.clientY - middleY) <= bounds.height * .2 && event.clientX < middleX;
  order.list.insertBefore(order.card, before ? target : target.nextSibling);
}

function finishDeviceOrder(event, save = true) {
  cancelDevicePress(event?.pointerId ?? null);
  const order = state.deviceOrder;
  if (!order || event && event.pointerId !== order.pointerId) return;
  const deviceIDs = [...order.list.querySelectorAll(':scope > .device-card')].map(card => card.dataset.deviceId);
  const changed = deviceIDs.some((id, index) => id !== order.originalIDs[index]);
  order.card.classList.remove('sorting-source');
  order.list.classList.remove('sorting');
  order.ghost.remove();
  document.body.classList.remove('device-ordering');
  $('device-order-hint').classList.add('hidden');
  state.deviceOrder = null;
  state.suppressDeviceClickUntil = Date.now() + 600;
  if (!save || !changed) {
    if (!save) renderDevices();
    return;
  }
  deviceIDs.forEach((id, index) => {
    const device = state.devices.find(candidate => candidate.id === id);
    if (device) device.sortOrder = (index + 1) * 10;
  });
  renderDevices();
  void persistDeviceOrder(order.groupID, deviceIDs);
}

async function persistDeviceOrder(groupID, deviceIDs) {
  try {
    await api('/api/stulp/devices/order', {
      method: 'PUT', body: JSON.stringify({ groupId: groupID, deviceIds: deviceIDs }),
    });
    toast('Tegelvolgorde opgeslagen');
  } catch (error) {
    toast(error.message, true);
    await load();
  }
}

// One ordered list decides every tile's single quick control/status. A
// capability suffix such as onoff.2 shares its base capability's priority.
const quickCapabilityPriority = [
  'alarm_smoke', 'alarm_fire', 'alarm_co', 'alarm_co2', 'alarm_vape', 'alarm_water', 'alarm_heat',
  'onoff', 'locked', 'garagedoor_closed', 'windowcoverings_state', 'homealarm_state',
  'alarm_motion', 'alarm_contact', 'alarm_glassbreak', 'alarm_vibration', 'alarm_generic', 'alarm_pressure', 'alarm_night',
  'speaker_playing', 'volume_mute', 'vacuumcleaner_state', 'dim',
  'target_temperature', 'measure_temperature', 'thermostat_mode',
  'measure_humidity', 'measure_co2', 'measure_co', 'measure_pm25', 'measure_luminance', 'measure_pressure', 'measure_noise',
  'measure_rain', 'measure_water', 'measure_wind_strength', 'measure_gust_strength', 'measure_wind_angle', 'measure_ultraviolet',
  'measure_battery', 'alarm_battery', 'alarm_tamper',
  'measure_power', 'meter_power', 'measure_current', 'measure_voltage', 'meter_water', 'meter_gas',
  'volume_set', 'speaker_track', 'speaker_artist', 'light_temperature', 'light_mode', 'light_hue', 'light_saturation',
  'windowcoverings_set', 'lock_mode', 'button',
];

const urgentAlarmCapabilities = new Set(['alarm_smoke', 'alarm_fire', 'alarm_co', 'alarm_co2', 'alarm_vape', 'alarm_water', 'alarm_heat']);

function baseCapabilityID(id) { return String(id || '').split('.')[0]; }
function unknownCapabilityValue(value) { return value === null || value === undefined || value === ''; }
function statelessCapability(id) {
  const base = baseCapabilityID(id);
  return base === 'speaker_prev' || base === 'speaker_next' || base === 'button';
}

// Wat een apparaat ís gaat voor wat het toevallig ook meet.
//
// De algemene lijst zet temperatuur hoog, en terecht: bij een thermostaat of een
// sensor is dat het antwoord. Bij een thuisbatterij is de celtemperatuur
// technisch juist en praktisch nutteloos.
//
// Vermogen staat hier vóór de lading, en dat is geen willekeur: de systeem-unit
// van zo'n installatie draagt het ladingspercentage zelf al, en die staat er
// naast. Twee tegels die allebei "98%" zeggen vertellen samen minder dan één die
// het percentage toont en één die zegt of er nu geladen of ontladen wordt.
const quickCapabilityByClass = {
  battery: ['measure_power', 'measure_battery', 'battery_charging_state'],
};

function quickCapability(device) {
  const capabilities = Object.values(device.capabilitiesObj || {});
  for (const wanted of [...(quickCapabilityByClass[device.class] || []), ...quickCapabilityPriority]) {
    const capability = capabilities.find(candidate => baseCapabilityID(candidate.id) === wanted);
    if (capability) return capability;
  }
  return capabilities.find(capability => baseCapabilityID(capability.id).startsWith('alarm_')) ||
    capabilities.find(capability => capability.setable && capability.type === 'boolean') ||
    capabilities.find(capability => baseCapabilityID(capability.id).startsWith('measure_')) ||
    capabilities.find(capability => baseCapabilityID(capability.id).startsWith('meter_')) || capabilities[0] || null;
}

function deviceQuickControl(device) {
  const capability = quickCapability(device);
  if (!capability) return null;
  const base = baseCapabilityID(capability.id);
  const title = localized(capability.title) || capability.id;
  if (!statelessCapability(capability.id) && unknownCapabilityValue(capability.value)) {
    const unknown = node('span', 'device-quick quick-value quick-unknown', '—');
    unknown.title = `${title}: status wordt opgehaald`;
    unknown.setAttribute('role', 'status');
    unknown.setAttribute('aria-label', unknown.title);
    return unknown;
  }

  // Een zonwering heeft één zinnige knop, en welke dat is hangt af van hoe hij
  // hangt: staat hij deels of geheel omlaag, dan wil je omhoog. De stand is uit
  // te lezen, dus dat hoeft niemand zelf te bedenken.
  if (base === 'windowcoverings_state' && capability.setable) {
    const position = Object.values(device.capabilitiesObj || {})
      .find(candidate => baseCapabilityID(candidate.id) === 'windowcoverings_set');
    const height = unknownCapabilityValue(position?.value) ? NaN : Number(position.value);
    // 1 is helemaal open, 0 is helemaal dicht. Zonder stand valt terug te vallen
    // op de laatste richting die de zonwering opging.
    const lowered = Number.isFinite(height) ? height < 1 : capability.value === 'down';
    const wanted = lowered ? 'up' : 'down';
    const label = lowered ? 'Omhoog' : 'Omlaag';
    const button = node('button', 'device-quick quick-action');
    button.type = 'button'; button.disabled = !device.available;
    button.title = Number.isFinite(height) ? `${title}: ${Math.round(height * 100)}% open — ${label}` : `${title}: ${label}`;
    button.setAttribute('aria-label', `${device.name} ${label.toLowerCase()}`);
    button.append(materialIcon(wanted === 'up' ? 'vertical_align_top' : 'vertical_align_bottom'));
    button.addEventListener('click', () => setCapability(device, capability, wanted));
    return button;
  }
  if (capability.type === 'boolean') {
    if (capability.setable) {
      const button = node('button', `device-quick quick-action ${capability.value ? 'active' : ''}`);
      button.type = 'button'; button.disabled = !device.available;
      const words = quickActionWords[base] || quickActionWords.default;
      button.title = `${title}: ${capability.value ? words.on : words.off}`;
      button.setAttribute('aria-label', `${device.name} ${capability.value ? words.turnOff : words.turnOn}`);
      button.append(quickActionIcon(base, Boolean(capability.value)));
      button.addEventListener('click', () => setCapability(device, capability, !capability.value));
      return button;
    }
    const active = Boolean(capability.value);
    if (base === 'alarm_contact') {
      const label = active ? 'Dicht' : 'Open';
      const status = node('span', `device-quick quick-status quick-contact-status ${active ? '' : 'open'}`);
      status.title = `${title}: ${label}`;
      status.setAttribute('role', 'img');
      status.setAttribute('aria-label', status.title);
      status.append(materialIcon(active ? 'lock' : 'lock_open'), node('span', 'sr-only', status.title));
      return status;
    }
    const stateLabels = {
      locked: active ? 'Op slot' : 'Open',
      garagedoor_closed: active ? 'Dicht' : 'Open',
      volume_mute: active ? 'Stil' : 'Geluid',
      speaker_playing: active ? 'Speelt' : 'Pauze',
    };
    if (stateLabels[base]) {
      const value = node('span', 'device-quick quick-value', stateLabels[base]);
      value.title = `${title}: ${stateLabels[base]}`;
      return value;
    }
    const status = node('span', `device-quick quick-status ${active ? 'active' : ''} ${active && urgentAlarmCapabilities.has(base) ? 'urgent' : ''}`);
    status.title = `${title}: ${active ? 'actief' : 'rustig'}`;
    status.setAttribute('role', 'img'); status.setAttribute('aria-label', status.title);
    status.append(node('span', 'quick-dot'), node('span', 'sr-only', status.title));
    return status;
  }
  const value = node('span', 'device-quick quick-value', formatQuickValue(capability));
  value.title = `${title}: ${formatValue(capability.value, capability.units)}`;
  return value;
}

// De woorden bij een aan/uit-knop. "Aan" en "uit" kloppen bij een lamp en niet
// bij een speler: die speelt of staat stil, en de knop zegt wat er gebeurt als
// je hem indrukt.
const quickActionWords = {
  default: { on: 'aan', off: 'uit', turnOn: 'aanzetten', turnOff: 'uitzetten' },
  speaker_playing: { on: 'speelt', off: 'gepauzeerd', turnOn: 'afspelen', turnOff: 'pauzeren' },
  volume_mute: { on: 'stil', off: 'geluid aan', turnOn: 'dempen', turnOff: 'geluid aanzetten' },
  locked: { on: 'op slot', off: 'open', turnOn: 'op slot doen', turnOff: 'van het slot doen' },
};

function quickActionIcon(base, active) {
  const icons = {
    locked: active ? 'lock' : 'lock_open',
    // Een transportknop toont wat hij doet, niet hoe het ervoor staat: speelt
    // hij, dan is de knop pauze. Andersom stond er pauze terwijl indrukken
    // pauzeerde -- precies verkeerd om.
    speaker_playing: active ? 'pause' : 'play_arrow',
    volume_mute: 'volume_off',
  };
  return materialIcon(icons[base] || 'power_settings_new');
}

function formatQuickValue(capability) {
  if (unknownCapabilityValue(capability.value)) return '—';
  const base = baseCapabilityID(capability.id);
  const numeric = Number(capability.value);
  if (!Number.isFinite(numeric)) return String(capability.value);
  if (base === 'dim' || base === 'windowcoverings_set') return `${Math.round(numeric * 100)}%`;
  const decimals = base === 'measure_temperature' || base === 'target_temperature' || base === 'measure_humidity' || base === 'measure_pressure' ? 1 : 0;
  const formatted = (Math.round(numeric * (10 ** decimals)) / (10 ** decimals)).toFixed(decimals).replace('.', ',').replace(/,0$/, '');
  const units = localized(capability.units);
  if (units === '%') return `${formatted}%`;
  if (units === '°C') return `${formatted}°`;
  return `${formatted}${units ? ` ${units}` : ''}`;
}

async function loadDeviceMedia(deviceID) {
  const device = state.devices.find(candidate => candidate.id === deviceID);
  if (!device || device.mediaLoading || device.mediaLoaded) return;
  device.mediaLoading = true;
  try {
	device.media = await api(`/api/stulp/devices/${encode(device.id)}/media`);
	device.mediaLoaded = true;
  } catch (_) {
	device.media = [];
	device.mediaLoaded = true;
  } finally {
	device.mediaLoading = false;
	if (state.openDeviceID === deviceID) renderDeviceOverview(device);
  }
}

function deviceClassIcon(deviceClass) {
  const iconClass = String(deviceClass || 'device').toLowerCase().replace(/[^a-z0-9_-]/g, '-');
  const icon = node('span', `device-icon device-icon-${iconClass}`);
  icon.setAttribute('aria-hidden', 'true');
  icon.append(materialIcon(deviceClassSymbol(deviceClass)));
  return icon;
}

function deviceClassSymbol(deviceClass) {
  return {
    light: 'lightbulb', socket: 'outlet', camera: 'videocam', lock: 'lock', sensor: 'sensors',
    weather: 'partly_cloudy_day',
    phone: 'phone_iphone',
    thermostat: 'thermostat', speaker: 'speaker', blinds: 'blinds', sunshade: 'blinds',
    windowcoverings: 'curtains', battery: 'battery_5_bar', solarpanel: 'solar_power',
    heatpump: 'heat_pump', evcharger: 'ev_station', scene: 'auto_awesome', other: 'devices_other',
  }[deviceClass] || 'devices_other';
}

function prepareDevicePopover(device) {
  state.openDeviceID = device.id;
  updateDevicePopoverHeader(device);
  renderDeviceOverview(device);
  renderDeviceConfiguration(device, state.drivers.find(item => item.id === device.driverId));
  showDeviceTab('overview');
  if (!device.mediaLoaded && !device.mediaLoading) void loadDeviceMedia(device.id);
  if (!$('device-popover').open) $('device-popover').showModal();
}

function updateDevicePopoverHeader(device) {
  $('device-popover-icon').replaceChildren(deviceClassIcon(device.class));
  $('device-popover-title').textContent = device.name;
  $('device-popover-status').textContent = device.available ? 'Beschikbaar' : device.unavailableMessage || 'Niet beschikbaar';
}

function showDeviceTab(tab) {
  state.deviceTab = tab === 'configuration' ? 'configuration' : 'overview';
  document.querySelectorAll('[data-device-tab]').forEach(button => {
    const active = button.dataset.deviceTab === state.deviceTab;
    button.classList.toggle('active', active);
    button.setAttribute('aria-selected', String(active));
    button.tabIndex = active ? 0 : -1;
  });
  $('device-overview-panel').classList.toggle('hidden', state.deviceTab !== 'overview');
  $('device-configuration-panel').classList.toggle('hidden', state.deviceTab !== 'configuration');
  if (state.deviceTab === 'configuration') $('device-config-name')?.focus();
}

function renderDeviceOverview(device) {
  const details = $('device-overview');
  if (!details || state.openDeviceID !== device.id) return;
  details.replaceChildren();
  const status = node('div', 'device-meta');
  status.append(node('span', `availability-dot ${device.available ? '' : 'off'}`), document.createTextNode(device.available ? 'Beschikbaar' : device.unavailableMessage || 'Niet beschikbaar'));
  status.append(node('span', '', device.manufacturer || device.appId), node('span', '', `Hardware: ${device.hardwareName || device.name}`), node('span', '', localized(device.class) || device.driverId));
  details.append(status);

  const controls = node('div', 'controls');
  Object.values(device.capabilitiesObj || {}).forEach(capability => controls.append(capabilityControl(device, capability)));
  if (controls.childElementCount) details.append(controls);
  else details.append(node('p', 'empty', 'Dit apparaat heeft geen bedienbare of meetbare mogelijkheden.'));

  renderDeviceMedia(device, details);

  const actions = node('div', 'row-actions');
  if (device.mediaLoading) actions.append(node('span', 'hint', 'Media laden…'));
  actions.append(actionButton('Verwijder', () => deleteDevice(device), 'danger'));
  details.append(actions);
}

// Het beeld van een camera bij elkaar: het stilstaande beeld waar je als eerste
// naar kijkt, en de weg naar live beeld er direct onder.
//
// Die weg zat eerder tussen de apparaatacties, als knop met de naam van de
// camera erop naast "Verwijder". Aan een knop met "Voordeur" erop is niet te
// zien dat er live beeld achter zit, dus werd hij nooit ingedrukt en leek de
// pagina alleen een plaatje te tonen. Nu staat er een speelknop bij het beeld,
// en is het beeld zelf ook aan te klikken -- dat is wat je bij een camera
// probeert.
function renderDeviceMedia(device, details) {
  const media = device.media || [];
  const stills = media.filter(item => item.kind === 'image');
  const videos = media.filter(item => item.kind === 'video');
  if (!stills.length && !videos.length) return;

  const frame = node('div', 'camera-still');
  const buttons = node('div', 'camera-actions');
  for (const still of stills) {
    const image = node('img');
    image.alt = still.title || device.name;
    image.loading = 'lazy';
    // De teller dwingt een verse ophaal af; zonder dat toont de browser het
    // beeld van de vorige keer.
    const refresh = () => { image.src = `/api/stulp/devices/${encode(device.id)}/media/${encode(still.slot)}/stream?kind=image&t=${Date.now()}`; };
    image.addEventListener('error', () => { image.replaceWith(node('p', 'empty', 'Geen beeld van deze camera.')); }, { once: true });
    if (videos.length) {
      image.classList.add('camera-live');
      image.title = 'Live beeld';
      image.addEventListener('click', () => startVideo(device, videos[0]));
    }
    frame.append(image);
    buttons.append(mediaButton('refresh', 'Ververs', refresh));
    refresh();
  }
  for (const video of videos) {
    // Eén stream heet gewoon "Live beeld". Heeft een camera er meer, dan is de
    // titel van de plugin het enige wat ze onderscheidt.
    const label = videos.length > 1 ? video.title || 'Live beeld' : 'Live beeld';
    buttons.append(mediaButton('play_arrow', label, () => startVideo(device, video), 'primary'));
  }
  frame.append(buttons);
  details.append(frame);
}

function mediaButton(icon, label, handler, className = '') {
  const button = actionButton('', handler, `button-with-icon ${className}`.trim());
  button.append(materialIcon(icon), document.createTextNode(label));
  return button;
}

function refreshOpenDevicePopover(device) {
  const popover = $('device-popover');
  if (!device || state.openDeviceID !== device.id || !popover.open) return;
  updateDevicePopoverHeader(device);
  renderDeviceOverview(device);
}

function openGroupEditor(group = null) {
  state.editingGroup = group ? { ...group } : null;
  $('group-title').textContent = group ? 'Groep bewerken' : 'Groep toevoegen';
  $('group-status').textContent = group ? 'De hele groepstak verhuist mee.' : 'Maak een hoofdgroep of plaats hem in een bestaande groep.';
  $('group-name').value = group?.name || '';
  const parent = $('group-parent');
  parent.replaceChildren();
  const root = node('option', '', 'Geen — hoofdgroep'); root.value = ''; parent.append(root);
  appendGroupOptions(parent, group?.parentId || '', group?.id || '');
  parent.value = group?.parentId || '';
  $('group-delete').classList.toggle('hidden', !group);
  $('group-dialog').showModal();
  $('group-name').focus();
}

async function saveGroup(event) {
  event.preventDefault();
  const group = state.editingGroup;
  $('group-status').textContent = 'Opslaan…';
  try {
    await api(group ? `/api/stulp/device-groups/${encode(group.id)}` : '/api/stulp/device-groups', {
      method: group ? 'PUT' : 'POST', body: JSON.stringify({ name: $('group-name').value.trim(), parentId: $('group-parent').value }),
    });
    $('group-dialog').close();
    await load();
    toast(group ? 'Groep bijgewerkt' : 'Groep toegevoegd');
  } catch (error) {
    $('group-status').textContent = error.message;
  }
}

async function deleteGroup() {
  const group = state.editingGroup;
  if (!group || !confirm(`Groep “${group.name}” verwijderen? Apparaten en subgroepen schuiven één niveau omhoog.`)) return;
  try {
    await api(`/api/stulp/device-groups/${encode(group.id)}`, { method: 'DELETE' });
    $('group-dialog').close();
    await load();
    toast('Groep verwijderd');
  } catch (error) { $('group-status').textContent = error.message; }
}

async function persistDeviceGroupOrder(groups) {
  try {
    await Promise.all(groups.map((group, position) => api(`/api/stulp/device-groups/${encode(group.id)}`, {
      method: 'PUT', body: JSON.stringify({ sortOrder: (position + 1) * 10 }),
    })));
    await load();
  } catch (error) { toast(error.message, true); }
}

async function moveDeviceGroupOrder(group, direction) {
  const groups = childGroups(group.parentId || '');
  const index = groups.findIndex(candidate => candidate.id === group.id);
  const target = index + direction;
  if (index < 0 || target < 0 || target >= groups.length) return;
  [groups[index], groups[target]] = [groups[target], groups[index]];
  await persistDeviceGroupOrder(groups);
}

async function setDeviceGroup(device, groupId) {
  if ((device.groupId || '') === groupId) return device;
  return api(`/api/stulp/devices/${encode(device.id)}/group`, { method: 'PUT', body: JSON.stringify({ groupId }) });
}

async function renameDevice(device, name) {
  if (name === device.name) return device;
  return api(`/api/manager/devices/device/${encode(device.id)}`, {
	method: 'PUT', body: JSON.stringify({ name }),
  });
}

function capabilityControl(device, capability) {
	const row = node('div', 'control');
	const base = baseCapabilityID(capability.id);
	row.append(node('span', '', localized(capability.title) || capability.id));
  // Vorige/volgende en generieke drukknoppen hebben expres geen state. Alle
  // andere controls wachten op hun eerste echte waarde, zodat onbekend nooit
  // per ongeluk als uit, open, pauze of nul wordt weergegeven.
  if (!statelessCapability(capability.id) && unknownCapabilityValue(capability.value)) {
    row.append(node('span', 'value', '—'), node('span'));
    return row;
  }
  if (!capability.setable) {
    row.append(node('span', 'value', formatValue(capability.value, capability.units)), node('span'));
    return row;
	}
	// Een schuif voor elk bedienbaar getal met een begin en een eind. Dat was
	// eerder alleen dim; een zonwering heeft precies dezelfde 0-tot-1 en kreeg
	// een invoerveld waar je "0.35" in moest typen.
	if (capability.type === 'number' && Number.isFinite(Number(capability.min)) && Number.isFinite(Number(capability.max))) {
		const min = Number(capability.min); const max = Number(capability.max);
		const control = node('div', 'percentage-control');
		const input = node('input'); input.type = 'range'; input.min = min; input.max = max; input.step = capability.step ?? 0.01;
		input.value = Number.isFinite(Number(capability.value)) ? capability.value : min;
		const output = node('output', 'percentage-value');
		// Van 0 tot 1 leest een percentage prettiger dan "0,35"; een andere
		// schaal toont gewoon zijn eigen getal.
		const percentage = min === 0 && max === 1;
		const show = () => {
			output.textContent = percentage
				? `${Math.round(Number(input.value) * 100)}%`
				: formatValue(Number(input.value), capability.units);
		};
		show(); input.addEventListener('input', show);
		input.addEventListener('change', () => setCapability(device, capability, Number(input.value)));
		control.append(input, output); row.append(control, node('span'));
		return row;
	}
	// Een richting kies je niet uit een lijst, die druk je in.
	if (base === 'windowcoverings_state' && Array.isArray(capability.values)) {
		const control = node('div', 'button-row covering-direction');
		const icons = { up: 'arrow_upward', idle: 'stop', down: 'arrow_downward' };
		for (const value of capability.values) {
			const label = localized(value.title || value.label) || value.id;
			const active = capability.value === value.id;
			const button = node('button', `covering-direction-button icon-button ${active ? 'active' : ''}`);
			button.type = 'button';
			button.title = label;
			button.setAttribute('aria-label', label);
			button.setAttribute('aria-pressed', String(active));
			button.append(materialIcon(icons[value.id] || 'radio_button_checked'), node('span', 'sr-only', label));
			button.addEventListener('click', () => setCapability(device, capability, value.id));
			control.append(button);
		}
		row.append(control, node('span'));
		return row;
	}
	let input;
  if (capability.type === 'boolean') {
    const mediaCommandIcons = { speaker_prev: 'skip_previous', speaker_next: 'skip_next' };
    if (mediaCommandIcons[base]) {
      const label = localized(capability.title) || (base === 'speaker_prev' ? 'Vorige' : 'Volgende');
      input = node('button', 'capability-icon-toggle icon-button');
      input.title = label;
      input.setAttribute('aria-label', label);
      input.append(materialIcon(mediaCommandIcons[base]), node('span', 'sr-only', label));
    } else if (base === 'speaker_playing') {
      const playing = Boolean(capability.value);
      const label = playing ? 'Pauzeren' : 'Afspelen';
      input = node('button', `capability-icon-toggle icon-button ${playing ? 'active' : ''}`);
      input.title = label;
      input.setAttribute('aria-label', label);
      input.setAttribute('aria-pressed', String(playing));
      input.append(materialIcon(playing ? 'pause' : 'play_arrow'), node('span', 'sr-only', label));
    } else if (base === 'onoff') {
      const enabled = Boolean(capability.value);
      const label = enabled ? 'Uitzetten' : 'Aanzetten';
      input = node('button', `capability-icon-toggle onoff-toggle icon-button ${enabled ? 'active' : ''}`);
      input.title = label;
      input.setAttribute('aria-label', label);
      input.setAttribute('aria-pressed', String(enabled));
      input.append(materialIcon(enabled ? 'toggle_on' : 'toggle_off'), node('span', 'sr-only', label));
    } else {
      input = node('button', '', capability.value ? 'Aan' : 'Uit');
    }
    input.addEventListener('click', () => setCapability(device, capability, mediaCommandIcons[base] ? true : !capability.value));
    row.append(input, node('span'));
    return row;
  }
  if (Array.isArray(capability.values)) {
    input = node('select');
    for (const value of capability.values) {
      const option = node('option', '', localized(value.title || value.label) || value.id);
      option.value = value.id;
      option.selected = value.id === capability.value;
      input.append(option);
    }
  } else {
    input = node('input');
    input.type = capability.type === 'number' ? 'number' : 'text';
    input.value = capability.value ?? '';
    if (capability.min !== undefined) input.min = capability.min;
    if (capability.max !== undefined) input.max = capability.max;
    if (capability.step !== undefined) input.step = capability.step;
  }
  const save = actionButton('Set', () => setCapability(device, capability, input.type === 'number' ? Number(input.value) : input.value));
  row.append(input, save);
  return row;
}

function formatValue(value, units) {
  if (unknownCapabilityValue(value)) return '—';
  return `${value}${units ? ` ${localized(units)}` : ''}`;
}
function actionButton(label, handler, className = '') {
  const button = node('button', className, label);
  button.type = 'button';
  button.addEventListener('click', handler);
  return button;
}
async function setCapability(device, capability, value) {
  try {
    await api(`/api/manager/devices/device/${encode(device.id)}/capability/${encode(capability.id)}`, {
      method: 'PUT', body: JSON.stringify({ value }),
    });
    // Hier stond de gevraagde waarde meteen als waarheid neergezet. Dat is
    // gokken: een opdracht is aangenomen, niet uitgevoerd. Het gevolg was
    // zichtbaar -- de knop sprong om, de eerste echte melding zette hem terug,
    // en een tel later sprong hij nog eens. Wat een apparaat doet komt van het
    // apparaat, en dat komt binnen over de gebeurtenissenstroom.
  } catch (error) { toast(error.message, true); }
}

// Stulp zelf hoort in dezelfde lijst als de apps: wat hij aan of uit heeft
// staan is net zo goed configuratie, en een tweede plek om te zoeken is een
// plek te veel. Hij staat bovenaan, is niet te verwijderen en niet uit te
// zetten -- hij ís de controller.
function renderStulp(list) {
  const row = node('article', 'row');
  const head = node('div', 'row-head');
  const name = node('div', 'name');
  name.append(node('strong', '', 'Stulp'),
    node('small', '', `de controller zelf · ${state.system.version || ''}`.trim()));
  head.append(name, node('span', 'status', 'running'));
  row.append(head);

  const on = Boolean(state.system.statistics);
  const line = node('div', 'control');
  line.append(node('span', '', 'Statistiek verzamelen'));
  const button = node('button', on ? 'active' : '', on ? 'Aan' : 'Uit');
  button.type = 'button';
  button.addEventListener('click', () => setSystem({ statistics: !on }));
  line.append(button, node('span'));
  row.append(line);

  row.append(node('p', 'hint', on
    ? `Elke capability krijgt drie ringen: een dag in vakken van tien minuten, een week van twee uur, een maand van vijf uur. Nu ${formatBytes(state.system.statisticsBytes || 0)} in geheugen, en dat groeit niet verder.`
    : 'Uit. Stulp leest niets mee en houdt niets bij; de grafiekroute zegt dat er niets verzameld wordt. Stroom eruit is statistiek weg — er is geen bewaarplek.'));

  // De eenheden waarin dit huis leest. Dit hoort bij Stulp en niet bij één app:
  // een thermometer van de warmtepomp en een van het weer horen dezelfde eenheid
  // te tonen, anders staat er in hetzelfde overzicht °C naast °F.
  //
  // De lijst komt van de kern, niet uit een lijstje hier: twee lijsten lopen uit
  // elkaar en dan biedt de pagina een keuze aan die niets doet.
  for (const quantity of state.system.unitsOffer || []) {
    const line = node('div', 'control');
    line.append(node('span', '', quantity.title));
    const select = node('select');
    if (quantity.hint) select.title = quantity.hint;
    for (const choice of quantity.options) {
      // Een keuze draagt zijn eigen naam: "Bft (Beaufort)", en bij vermogen
      // "zoals de app meldt" — die heeft geen eenheid en is toch een keuze.
      const option = node('option', '', choice.title || choice.unit);
      option.value = choice.unit;
      if (((state.system.units || {})[quantity.name] || '') === choice.unit) option.selected = true;
      select.append(option);
    }
    select.addEventListener('change', () => setSystem({ units: { [quantity.name]: select.value } }));
    line.append(select, node('span'));
    row.append(line);
  }
  row.append(node('p', 'hint', 'Alleen hoe je het leest. Wat Stulp bewaart blijft °C, m/s, mm, km en hPa — '
    + 'dus een andere keuze herschrijft geen geschiedenis en verschuift geen Flow-drempel. '
    + 'Procenten, watts en kilowattuur staan er niet bij: die zijn overal hetzelfde.'));
  list.append(row);
}

function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} kB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

async function setSystem(patch) {
  try {
    const answer = await api('/api/stulp/system', { method: 'PUT', body: JSON.stringify(patch) });
    state.system = { ...answer, version: state.system.version };
    // Een andere eenheid raakt elke tegel, elke kaart en elke Flow-drempel, en
    // die komen allemaal van de server. Dus opnieuw ophalen in plaats van hier
    // gaan rekenen: dan is er één plek waar de omrekening staat.
    if (patch.units) await load(); else renderApps();
  } catch (error) { toast(error.message, true); }
}

function renderApps() {
  const list = $('apps');
  list.replaceChildren();
  renderStulp(list);
  if (!state.apps.length) {
    return list.append(node('p', 'empty',
      'Nog geen apps. Een app komt binnen doordat je hem neerzet: hij meldt zich hier aan en jij installeert hem.'));
  }
  for (const app of state.apps) {
    const row = node('article', 'row');
    const head = node('div', 'row-head');
    const name = node('div', 'name');
    // Zonder versie (net hersteld, announce nog onderweg) geen kale punt: de
    // versie is wat de app zégt, en zolang hij niets zei is er niets te tonen.
    name.append(node('strong', '', app.name || app.id),
      node('small', '', app.version ? `${app.id} · ${app.version}` : app.id));
    // Aangeboden is niet uitgeschakeld: hij heeft zich gemeld en wacht op jou.
    head.append(name, node('span', `status ${app.offered ? 'off' : app.state === 'running' ? '' : 'off'}`,
      app.offered ? 'aangemeld' : app.state));
    row.append(head);
    if (app.offered) {
      row.append(node('p', 'hint',
        'Deze app heeft zich aangemeld en draait nog niet. Installeer hem om hem toe te laten.'));
      const offer = node('div', 'row-actions');
      offer.append(actionButton('Installeren', () => installApp(app), 'primary'));
      offer.append(actionButton('Weiger', () => uninstallApp(app), 'danger'));
      row.append(offer);
      list.append(row);
      continue;
    }
	if (app.crashedMessage) row.append(node('p', 'app-error', app.crashedMessage));
	if (app.retryAt) row.append(node('p', 'flow-last-run', `Automatische herstart ${new Date(app.retryAt).toLocaleTimeString()} · poging ${Number(app.crashedCount || 0) + 1}`));
    const actions = node('div', 'row-actions');
    if (app.settings) actions.append(actionButton('Configureer', () => openAppSettings(app), 'primary'));
    actions.append(actionButton(app.enabled ? 'Uitschakelen' : 'Inschakelen', () => toggleApp(app)));
    if (app.enabled) actions.append(actionButton('Herstart', () => restartApp(app)));
    actions.append(actionButton('Verwijder', () => uninstallApp(app), 'danger'));
    row.append(actions);
    list.append(row);
  }
}



function appImpact(app) {
  const devices = state.devices.filter(device => device.appId === app.id);
  const deviceIds = new Set(devices.map(device => device.id));
  const flows = state.flows.filter(flow => flowSteps(flow).some(step =>
    step.appId === app.id || usesDevice(step.args, deviceIds) || usesDevice(step.state, deviceIds)));
  return { devices, flows };
}

function flowSteps(flow) {
  if (flow.nodes?.length) return flow.nodes.map(flowNode => flowNode.step || {});
  return [flow.trigger, ...(flow.conditions || []), ...(flow.actions || [])].filter(Boolean);
}

function usesDevice(value, deviceIds) {
  if (Array.isArray(value)) return value.some(child => usesDevice(child, deviceIds));
  if (!value || typeof value !== 'object') return false;
  if (typeof value.$device === 'string' && deviceIds.has(value.$device)) return true;
  return Object.values(value).some(child => usesDevice(child, deviceIds));
}

function openAppSettings(app) {
  state.settingsAppId = app.id;
  $('settings-title').textContent = app.name || app.id;
  $('settings-status').textContent = 'Settings laden…';
  $('settings-frame').src = `/app-ui/${encode(app.id)}/settings/`;
  $('settings-dialog').showModal();
}
async function toggleApp(app) {
  try {
    await api(`/api/manager/apps/app/${encode(app.id)}/${app.enabled ? 'disable' : 'enable'}`, { method: 'PUT' });
    await load();
  } catch (error) { toast(error.message, true); }
}
async function restartApp(app) {
  try { await api(`/api/manager/apps/app/${encode(app.id)}/restart`, { method: 'POST' }); await load(); }
  catch (error) { toast(error.message, true); }
}
async function uninstallApp(app) {
  const impact = appImpact(app);
  const consequences = [];
  if (impact.devices.length) {
    consequences.push(`${impact.devices.length} apparaat${impact.devices.length === 1 ? '' : 'en'} verdwijnt: ${impact.devices.map(device => device.name).join(', ')}`);
  }
  if (impact.flows.length) {
    consequences.push(`${impact.flows.length} Flow${impact.flows.length === 1 ? '' : 's'} wordt uitgeschakeld: ${impact.flows.map(flow => flow.name).join(', ')}`);
  }
  const question = [`${app.name || app.id} verwijderen?`, ...consequences, 'Dit kan niet ongedaan worden gemaakt.'].join('\n\n');
  if (!confirm(question)) return;
  try {
    const removed = await api(`/api/manager/apps/app/${encode(app.id)}`, { method: 'DELETE' });
    await load();
    toast(removed.warning || `${removed.name || removed.id} is verwijderd`, Boolean(removed.warning));
  } catch (error) { toast(error.message, true); }
}

async function installApp(app) {
  try {
    const installed = await api(`/api/stulp/apps/${encode(app.id)}/install`, { method: 'POST' });
    await load();
    toast(`${installed.name || installed.id} is geïnstalleerd en mag nu draaien`);
  } catch (error) { toast(error.message, true); }
}

async function downloadBackup() {
	const button = $('download-backup');
	button.disabled = true;
	try {
		const response = await fetch('/api/stulp/backup');
		if (!response.ok) {
			const error = await response.json().catch(() => null);
			throw new Error(error?.error_description || `HTTP ${response.status}`);
		}
		const link = document.createElement('a');
		link.href = URL.createObjectURL(await response.blob());
		link.download = `stulp-backup-${new Date().toISOString().slice(0, 10)}.zip`;
		document.body.append(link); link.click(); link.remove(); URL.revokeObjectURL(link.href);
		toast('Backup gedownload');
	} catch (error) { toast(error.message, true); }
	finally { button.disabled = false; }
}

function openRestore() {
	$('restore-form').reset();
	$('restore-file-detail').textContent = 'Kies het .zip-bestand dat door Stulp is gemaakt.';
	$('restore-dialog').showModal();
}

function describeRestoreFile() {
	const file = $('restore-file').files[0];
	$('restore-file-detail').textContent = file
		? `${file.name} · ${new Intl.NumberFormat('nl-NL', { maximumFractionDigits: 1 }).format(file.size / 1048576)} MB`
		: 'Kies het .zip-bestand dat door Stulp is gemaakt.';
}

async function restoreBackup(event) {
	event.preventDefault();
	const file = $('restore-file').files[0];
	if (!file || !$('restore-confirm').checked) return;
	const button = $('restore-submit');
	button.disabled = true;
	button.textContent = 'Controleren en herstellen…';
	try {
		const response = await fetch('/api/stulp/restore', {
			method: 'POST', headers: { 'Content-Type': 'application/zip' }, body: file,
		});
		const result = await response.json().catch(() => null);
		if (!response.ok) throw new Error(result?.error_description || result?.error || `HTTP ${response.status}`);
		$('restore-dialog').close();
		await load();
		toast(result?.warning || 'Backup hersteld; apps worden opnieuw verbonden', Boolean(result?.warning));
	} catch (error) {
		toast(error.message, true);
	} finally {
		button.disabled = false;
		button.textContent = 'Backup herstellen';
	}
}

// ---- Meldingen op een toestel ----
//
// Een telefoon is een device: je voegt hem toe met "Device toevoegen", en de
// koppelstap hieronder bindt deze browser aan dat device. Wat er verder met
// meldingen gebeurt is de plugin com.stulp.notify, niet deze pagina.
//
// Wat hier wél moet staan is de aanmelding zelf. Drie dingen moeten kloppen
// voordat een browser een abonnement mag aanvragen, en geen ervan gaat over
// Stulp: een beveiligde verbinding, een service worker en toestemming van de
// gebruiker. Ontbreekt er één, dan zegt de koppelstap welke -- dat is beter dan
// een knop die niets doet.

const iOSLike = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
const standaloneApp = navigator.standalone === true || window.matchMedia('(display-mode: standalone)').matches;

function pushBlocker() {
  if (!window.isSecureContext) return 'Alleen via https of op localhost. Zet Stulp achter een https-adres met een certificaat dat je telefoon vertrouwt.';
  // Deze volgorde is het hele punt. Safari geeft de push-API niet aan een
  // tabblad maar alleen aan een webapp op het beginscherm -- in een tabblad
  // bestaan Notification en PushManager domweg niet. Stond de test hieronder
  // eerst, dan kreeg juist de iPhone te horen dat hij geen meldingen kan
  // ontvangen, terwijl hij dat prima kan zodra Stulp op het beginscherm staat.
  if (iOSLike && !standaloneApp) {
    return 'Op een iPhone of iPad mogen alleen webapps meldingen ontvangen. Voeg Stulp toe via Deel → Zet op beginscherm, open hem daar en koppel dit apparaat opnieuw.';
  }
  if (!('serviceWorker' in navigator) || !('PushManager' in window) || !('Notification' in window)) {
    return iOSLike
      ? 'Deze iOS-versie kan nog geen pushberichten ontvangen; dat kan vanaf iOS 16.4.'
      : 'Deze browser kan geen pushberichten ontvangen.';
  }
  if (Notification.permission === 'denied') return 'Meldingen staan geblokkeerd voor deze site. Zet ze aan in de instellingen van je browser.';
  return '';
}

function base64UrlToBytes(value) {
  const standard = value.replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(standard + '='.repeat((4 - standard.length % 4) % 4));
  return Uint8Array.from(raw, character => character.charCodeAt(0));
}

function bytesToBase64Url(buffer) {
  let binary = '';
  for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// suggestedPhoneName is een voorstel, geen antwoord: "iPhone" zegt niets als er
// drie in huis zijn, dus typt de gebruiker er zijn eigen naam over.
function suggestedPhoneName() {
  if (/iPhone/.test(navigator.userAgent)) return 'iPhone';
  if (iOSLike) return 'iPad';
  if (/Android/.test(navigator.userAgent)) return 'Android';
  if (/Macintosh/.test(navigator.userAgent)) return 'Mac';
  if (/Windows/.test(navigator.userAgent)) return 'Windows';
  return 'Browser';
}

async function openNotifications() {
	const list = $('notifications');
	list.replaceChildren(node('p', 'empty', 'Laden…'));
	$('notifications-dialog').showModal();
	try {
		const notifications = Object.values(await api('/api/manager/notifications/notification'));
		list.replaceChildren();
		if (!notifications.length) return list.append(node('p', 'empty', 'Nog geen meldingen.'));
		for (const notification of notifications) {
			const row = node('article', 'notification-row');
			const copy = node('div', 'name');
			const owner = notification.appId === 'com.stulp.matter' ? 'Stulp' : state.apps.find(app => app.id === notification.appId)?.name || notification.appId;
			copy.append(node('strong', '', notification.excerpt), node('small', '', `${owner} · ${new Date(notification.createdAt).toLocaleString()}`));
			row.append(copy, actionButton('Wis', async () => {
				try { await api(`/api/manager/notifications/notification/${encode(notification.id)}`, { method: 'DELETE' }); row.remove(); }
				catch (error) { toast(error.message, true); }
			}));
			list.append(row);
		}
	} catch (error) { list.replaceChildren(node('p', 'app-error', error.message)); }
}

function sceneStateKey(deviceID, capabilityID) { return `${deviceID}\u0000${capabilityID}`; }

function sceneWritableCapabilities(device) {
  if (device.class === 'scene' || device.appId === nativeSceneAppID) return [];
  return Object.values(device.capabilitiesObj || {}).filter(capability =>
    capability.setable && capability.getable !== false && !statelessCapability(capability.id));
}

function formatSceneValue(capability, value) {
  const choice = capabilityChoices(capability)?.find(candidate => String(candidate.id) === String(value));
  if (choice) return choice.title;
  return formatValue(value, capability?.units);
}

function sceneStateDescription(wanted) {
  const device = state.devices.find(candidate => candidate.id === wanted.deviceId);
  const capability = device?.capabilitiesObj?.[wanted.capabilityId];
  const deviceName = device?.name || 'Ontbrekend apparaat';
  const title = localized(capability?.title) || wanted.capabilityId;
  return `${deviceName}: ${title} → ${formatSceneValue(capability || {}, wanted.value)}`;
}

function openScene(existing = null) {
  if ($('pair-dialog').open) $('pair-dialog').close();
  if ($('device-popover').open) $('device-popover').close();
  state.editingScene = existing
    ? JSON.parse(JSON.stringify(existing))
    : { name: '', states: [] };
  state.editingScene.states ||= [];
  $('scene-title').textContent = existing ? 'Scene configureren' : 'Scene toevoegen';
  $('scene-name').value = state.editingScene.name || '';
  renderSceneEditor();
  $('scene-dialog').showModal();
  $('scene-name').focus();
}

function sceneDefaultValue(capability) {
  if (!unknownCapabilityValue(capability.value)) return capability.value;
  const choices = capabilityChoices(capability);
  if (choices?.length) return choices[0].id;
  if (capability.type === 'number') return Number.isFinite(Number(capability.min)) ? Number(capability.min) : 0;
  return '';
}

function sceneTarget(deviceID, capabilityID) {
  return state.editingScene.states.find(wanted => wanted.deviceId === deviceID && wanted.capabilityId === capabilityID);
}

function setSceneTarget(target, selected) {
  const key = sceneStateKey(target.deviceId, target.capabilityId);
  state.editingScene.states = state.editingScene.states.filter(wanted => sceneStateKey(wanted.deviceId, wanted.capabilityId) !== key);
  if (selected) state.editingScene.states.push(target);
  updateSceneSelectionSummary();
}

function updateSceneSelectionSummary() {
  const states = state.editingScene?.states || [];
  const devices = new Set(states.map(wanted => wanted.deviceId)).size;
  $('scene-selection-summary').textContent = states.length
    ? `${states.length} ${states.length === 1 ? 'stand' : 'standen'} gekozen op ${devices} ${devices === 1 ? 'apparaat' : 'apparaten'}.`
    : 'Nog niets gekozen — deze scene verandert nog niets.';
}

function sceneValueEditor(capability, target, enabled) {
  const wrapper = node('div', `scene-value ${enabled ? '' : 'disabled'}`.trim());
  let input;
  const choices = capabilityChoices(capability);
  if (choices) {
    input = node('select');
    for (const choice of choices) {
      const option = node('option', '', choice.title); option.value = String(choice.id); input.append(option);
    }
    input.value = String(target.value);
    input.addEventListener('change', () => { target.value = capabilityChoiceValue(input.value, capability); });
    wrapper.append(input);
  } else if (capability.type === 'number' && Number(capability.min) === 0 && Number(capability.max) === 1) {
    const range = node('div', 'scene-range');
    input = node('input'); input.type = 'range'; input.min = 0; input.max = 1; input.step = capability.step ?? 0.01; input.value = target.value;
    const output = node('output');
    const show = () => { output.textContent = `${Math.round(Number(input.value) * 100)}%`; };
    show();
    input.addEventListener('input', () => { target.value = Number(input.value); show(); });
    range.append(input, output); wrapper.append(range);
  } else {
    input = node('input'); input.type = capability.type === 'number' ? 'number' : 'text'; input.value = target.value ?? '';
    if (capability.min !== undefined) input.min = capability.min;
    if (capability.max !== undefined) input.max = capability.max;
    if (capability.step !== undefined) input.step = capability.step;
    input.addEventListener('input', () => { target.value = capabilityChoiceValue(input.value, capability); });
    wrapper.append(input);
  }
  const setEnabled = selected => {
    wrapper.classList.toggle('disabled', !selected);
    input.disabled = !selected;
    input.required = selected;
  };
  setEnabled(enabled);
  return { element: wrapper, setEnabled };
}

function renderSceneCapability(device, capability) {
  const row = node('div', 'scene-capability');
  let target = sceneTarget(device.id, capability.id) || {
    deviceId: device.id, capabilityId: capability.id, value: sceneDefaultValue(capability),
  };
  const selected = Boolean(sceneTarget(device.id, capability.id));
  const toggle = node('label', 'scene-capability-toggle');
  const checkbox = node('input'); checkbox.type = 'checkbox'; checkbox.checked = selected;
  toggle.append(checkbox, node('span', '', localized(capability.title) || capability.id));
  const editor = sceneValueEditor(capability, target, selected);
  checkbox.addEventListener('change', () => {
    if (checkbox.checked) {
      setSceneTarget(target, true);
    } else {
      setSceneTarget(target, false);
    }
    editor.setEnabled(checkbox.checked);
  });
  row.append(toggle, editor.element);
  return row;
}

function captureSceneDevice(device) {
  const capabilities = sceneWritableCapabilities(device);
  const ids = new Set(capabilities.map(capability => sceneStateKey(device.id, capability.id)));
  state.editingScene.states = state.editingScene.states.filter(wanted => !ids.has(sceneStateKey(wanted.deviceId, wanted.capabilityId)));
  let captured = 0;
  for (const capability of capabilities) {
    if (unknownCapabilityValue(capability.value)) continue;
    state.editingScene.states.push({ deviceId: device.id, capabilityId: capability.id, value: capability.value });
    captured++;
  }
  renderSceneEditor();
  if (!captured) toast(`${device.name} heeft nog geen bekende standen.`, true);
}

function renderSceneDevice(device, capabilities) {
  const article = node('article', 'scene-device');
  const head = node('div', 'scene-device-head');
  const symbol = node('span', 'scene-device-symbol'); symbol.append(materialIcon(deviceClassSymbol(device.class)));
  const copy = node('span', 'scene-device-name');
  copy.append(node('strong', '', device.name), node('small', '', device.available ? `${capabilities.length} schrijfbare standen` : 'Niet beschikbaar · laatst bekende standen'));
  const capture = actionButton('Huidige standen', () => captureSceneDevice(device), 'scene-device-capture');
  head.append(symbol, copy, capture); article.append(head);
  const list = node('div', 'scene-capabilities');
  for (const capability of capabilities) list.append(renderSceneCapability(device, capability));
  article.append(list);
  return article;
}

function renderMissingSceneStates(known) {
  const missing = (state.editingScene.states || []).filter(wanted => !known.has(sceneStateKey(wanted.deviceId, wanted.capabilityId)));
  if (!missing.length) return null;
  const section = node('section', 'scene-device-group');
  section.append(node('h3', '', 'Niet meer beschikbaar'));
  const article = node('article', 'scene-device scene-missing');
  const head = node('div', 'scene-device-head');
  const symbol = node('span', 'scene-device-symbol'); symbol.append(materialIcon('warning'));
  const copy = node('span', 'scene-device-name'); copy.append(node('strong', '', 'Scene bevat verdwenen standen'), node('small', '', 'Verwijder ze of herstel het apparaat voordat je opslaat.'));
  head.append(symbol, copy); article.append(head);
  for (const wanted of missing) {
    const row = node('div', 'scene-missing-state');
    row.append(node('span', '', `${wanted.deviceId} · ${wanted.capabilityId}`), actionButton('Verwijder', () => {
      setSceneTarget(wanted, false); renderSceneEditor();
    }, 'danger small'));
    article.append(row);
  }
  section.append(article); return section;
}

function renderSceneEditor() {
  const target = $('scene-devices'); target.replaceChildren();
  const known = new Set();
  const byLocation = new Map();
  for (const device of state.devices) {
    const capabilities = sceneWritableCapabilities(device);
    if (!capabilities.length) continue;
    for (const capability of capabilities) known.add(sceneStateKey(device.id, capability.id));
    const group = state.deviceGroups.find(candidate => candidate.id === device.groupId);
    const location = group ? groupPath(group.id) : 'Zonder groep';
    if (!byLocation.has(location)) byLocation.set(location, []);
    byLocation.get(location).push({ device, capabilities });
  }
  const orderedLocations = [
    ...deviceGroupsInDisplayOrder().map(group => groupPath(group.id)).filter(location => byLocation.has(location)),
    ...(byLocation.has('Zonder groep') ? ['Zonder groep'] : []),
  ];
  for (const location of [...new Set(orderedLocations)]) {
    const section = node('section', 'scene-device-group'); section.append(node('h3', '', location));
    const entries = byLocation.get(location).sort((left, right) => compareDevices(left.device, right.device));
    for (const entry of entries) section.append(renderSceneDevice(entry.device, entry.capabilities));
    target.append(section);
  }
  const missing = renderMissingSceneStates(known);
  if (missing) target.append(missing);
  if (!target.childElementCount) target.append(node('p', 'empty', 'Nog geen apparaten met een schrijfbare, uitleesbare status.'));
  updateSceneSelectionSummary();
}

function captureWholeScene() {
  const captured = [];
  let skipped = 0;
  for (const device of state.devices) {
    for (const capability of sceneWritableCapabilities(device)) {
      if (unknownCapabilityValue(capability.value)) { skipped++; continue; }
      captured.push({ deviceId: device.id, capabilityId: capability.id, value: capability.value });
    }
  }
  state.editingScene.states = captured;
  renderSceneEditor();
  toast(`${captured.length} huidige standen gekozen${skipped ? ` · ${skipped} nog onbekend` : ''}`);
}

async function saveScene(event) {
  event.preventDefault();
  const scene = state.editingScene;
  scene.name = $('scene-name').value.trim();
  if (!scene.states?.length) return toast('Kies ten minste één stand voor deze scene.', true);
  if (!$('scene-form').reportValidity()) return;
  try {
    const path = scene.id ? `/api/stulp/scenes/${encode(scene.id)}` : '/api/stulp/scenes';
    const saved = await api(path, { method: scene.id ? 'PUT' : 'POST', body: JSON.stringify(scene) });
    $('scene-dialog').close();
    await load();
    const device = state.devices.find(candidate => candidate.data?.sceneId === saved.id);
    if (device) {
      prepareDevicePopover(device);
      showDeviceTab('configuration');
    }
    toast(scene.id ? 'Sceneconfiguratie opgeslagen' : 'Scene-apparaat toegevoegd');
  } catch (error) { toast(error.message, true); }
}

function renderFlows() {
  const list = $('flows');
  list.replaceChildren();
  if (!state.flows.length) return list.append(node('p', 'empty', 'Nog geen Flows. Maak je eerste ALS → EN → DAN-regel.'));
  for (const flow of state.flows) {
    const row = node('article', 'overview-card flow-overview-card');
    const head = node('div', 'overview-card-head flow-card-head');
    const icon = node('span', 'overview-card-icon flow-card-icon'); icon.append(materialIcon('account_tree'));
    const name = node('div', 'overview-card-copy flow-card-copy');
    const counts = flowNodeCounts(flow);
    name.append(node('strong', '', flow.name), node('small', '', `${counts.trigger} ALS · ${counts.condition} EN · ${counts.action} DAN · ${flow.edges?.length || 0} verbindingen`));
    head.append(icon, name, node('span', `overview-card-status ${flow.enabled ? 'active' : ''}`.trim(), flow.enabled ? 'Actief' : 'Uit'));
    row.append(head);
    if (flow.lastError) row.append(node('p', 'app-error', flow.lastError));
    else if (flow.lastRunAt) row.append(node('p', 'flow-last-run', `Laatst uitgevoerd: ${new Date(flow.lastRunAt).toLocaleString()}`));
    const actions = node('div', 'overview-card-actions row-actions flow-card-actions');
    actions.append(actionButton('Bewerk', () => openFlow(flow), 'primary'));
    actions.append(actionButton('Test', () => runFlow(flow)));
    actions.append(actionButton(flow.enabled ? 'Uitschakelen' : 'Inschakelen', () => toggleFlow(flow)));
    actions.append(actionButton('Verwijder', () => deleteFlow(flow), 'danger'));
    row.append(actions); list.append(row);
  }
}

function openFlow(existing = null) {
  if (existing) {
    state.editingFlow = JSON.parse(JSON.stringify(existing));
  } else {
    state.editingFlow = { name: '', enabled: true, nodes: [], edges: [] };
  }
  state.editingFlow.nodes ||= [];
  state.editingFlow.edges ||= [];
  $('flow-title').textContent = existing ? 'Flow bewerken' : 'Nieuwe Flow';
  $('flow-name').value = state.editingFlow.name || '';
  $('flow-enabled').checked = state.editingFlow.enabled !== false;
  $('flow-test').disabled = !state.editingFlow.id;
  $('flow-test').title = state.editingFlow.id ? 'Sla wijzigingen op en voer deze Flow direct uit' : 'Sla de Flow eerst op';
  renderFlowEditor();
  $('flow-dialog').showModal();
  scheduleFlowConnections();
  $('flow-name').focus();
}

function renderFlowEditor() {
  const flow = state.editingFlow;
  if (!flow) return;
  sizeFlowSpace();
  const target = $('flow-nodes');
  target.replaceChildren();
  flow.nodes.forEach(flowNode => target.append(renderFlowNode(flowNode)));
  $('flow-empty').classList.toggle('hidden', flow.nodes.length > 0);
  const tokens = flowTriggerTokens().map(token => `{{${token.name}}}`);
  $('flow-tokens').textContent = tokens.length ? `Beschikbare triggerwaarden: ${tokens.join(', ')}` : 'Actievelden mogen een vaste waarde of {{token}} bevatten.';
  watchFlowLayout();
  scheduleFlowConnections();
}

function sizeFlowSpace() {
  const space = $('flow-space');
  const canvas = $('flow-canvas');
  const nodes = state.editingFlow?.nodes || [];
  const furthestX = nodes.reduce((maximum, flowNode) => Math.max(maximum, Number(flowNode.x) || 0), 0);
  const furthestY = nodes.reduce((maximum, flowNode) => Math.max(maximum, Number(flowNode.y) || 0), 0);
  space.style.width = `${Math.max(2400, canvas.clientWidth, furthestX + 520)}px`;
  space.style.height = `${Math.max(1400, canvas.clientHeight, furthestY + 420)}px`;
}

function renderFlowNode(flowNode) {
  const kind = flowNodeKind(flowNode);
  const step = flowNode.step;
  step.args ||= {};
  const wrapper = node('article', `flow-step ${kind}-card`);
  wrapper.dataset.kind = kind;
  wrapper.dataset.nodeId = flowNode.id;
  wrapper.style.left = `${flowNode.x}px`;
  wrapper.style.top = `${flowNode.y}px`;
  if (kind !== 'trigger') {
    const input = node('span', 'flow-port flow-port-in');
    input.dataset.nodeId = flowNode.id;
    input.title = 'Verbinding hierheen';
    wrapper.append(input);
  }
  const output = node('span', 'flow-port flow-port-out');
  output.dataset.nodeId = flowNode.id;
  output.title = 'Sleep een verbinding vanaf hier';
  output.addEventListener('pointerdown', event => startFlowConnection(event, flowNode.id));
  wrapper.append(output);

  const top = node('div', 'flow-step-top');
  const handle = materialIcon('drag_indicator', 'flow-drag-handle');
  handle.title = 'Sleep over het canvas';
  handle.addEventListener('pointerdown', event => startFlowNodeMove(event, flowNode, wrapper));
  top.append(handle);
  const badge = node('span', `flow-kind ${kind}-kind`, kind === 'trigger' ? 'ALS' : kind === 'condition' ? 'EN' : 'DAN');
  const identity = node('div', 'flow-card-identity');
  const card = cardForStep(step);
  const device = deviceForStep(step, card);
  identity.append(node('small', 'flow-card-device', device ? flowDeviceLabel(device) : card?.appName || step.appId));
  const title = node('div', 'flow-card-title', flowStepSentence(card, step));
  if (card?.hint) title.title = card.hint;
  identity.append(title);
  top.append(badge, identity);
  if (kind === 'condition') {
    const inverted = node('label', 'inline-check compact-check');
    const checkbox = node('input'); checkbox.type = 'checkbox'; checkbox.checked = Boolean(step.inverted);
    checkbox.addEventListener('change', () => { step.inverted = checkbox.checked; });
    inverted.append(checkbox, document.createTextNode(' Niet'));
    top.append(inverted);
  }
  const controls = node('div', 'flow-step-actions');
  controls.append(compactIconButton('delete', 'Verwijderen', () => removeFlowNode(flowNode.id), 'danger'));
  top.append(controls);
  wrapper.append(top);
  const body = node('div', 'flow-step-body');
  const argumentsBlock = node('div', 'flow-arguments');
  // Arguments depend on each other: the capability list comes from the
  // chosen device, and the value widget from the chosen capability. Redraw
  // the block instead of leaving stale choices behind.
  const redrawArguments = () => {
    argumentsBlock.replaceChildren();
    for (const argument of card?.args || []) {
      if (argument.type !== 'device') argumentsBlock.append(renderFlowArgument(step, card, argument, redrawArguments));
    }
  };
  redrawArguments();
  if (argumentsBlock.childElementCount) body.append(argumentsBlock);
  if (body.childElementCount) wrapper.append(body);
  return wrapper;
}

function flowDeviceLabel(device) {
  const group = state.deviceGroups.find(candidate => candidate.id === device.groupId);
  const location = group ? groupPath(group.id) : 'Zonder groep';
  return `Apparaat: (${location}) ${device.name}`;
}

function iconButton(icon, title, handler, className = '') {
  const button = actionButton('', handler, `icon-button ${className}`.trim());
  button.title = title;
  button.setAttribute('aria-label', title);
  button.append(materialIcon(icon));
  return button;
}

function compactIconButton(icon, title, handler, className = '') {
  return iconButton(icon, title, handler, `compact-icon-button ${className}`.trim());
}

function startFlowNodeMove(event, flowNode, wrapper) {
  if (event.button !== 0) return;
  event.preventDefault();
  event.stopPropagation();
  state.flowMove = {
    id: flowNode.id, pointerId: event.pointerId, startX: event.clientX, startY: event.clientY,
    nodeX: flowNode.x, nodeY: flowNode.y, wrapper,
  };
  wrapper.setPointerCapture(event.pointerId);
  wrapper.classList.add('dragging');
}

function moveFlowNode(event) {
  const moving = state.flowMove;
  if (!moving || event.pointerId !== moving.pointerId) return;
  const flowNode = state.editingFlow.nodes.find(item => item.id === moving.id);
  if (!flowNode) return;
  const space = $('flow-space');
  flowNode.x = Math.max(20, Math.min(space.clientWidth - 308, moving.nodeX + event.clientX - moving.startX));
  flowNode.y = Math.max(20, Math.min(space.clientHeight - 120, moving.nodeY + event.clientY - moving.startY));
  moving.wrapper.style.left = `${flowNode.x}px`;
  moving.wrapper.style.top = `${flowNode.y}px`;
  scheduleFlowConnections();
}

function finishFlowNodeMove(event) {
  const moving = state.flowMove;
  if (!moving || event.pointerId !== moving.pointerId) return;
  moving.wrapper.classList.remove('dragging');
  state.flowMove = null;
  sizeFlowSpace();
  scheduleFlowConnections();
}

function startFlowConnection(event, sourceID) {
  if (event.button !== 0) return;
  event.preventDefault();
  event.stopPropagation();
  state.flowLink = { sourceID, pointerId: event.pointerId };
  event.currentTarget.setPointerCapture(event.pointerId);
  updateFlowConnectionPreview(event);
}

function updateFlowConnectionPreview(event) {
  const linking = state.flowLink;
  if (!linking || event.pointerId !== linking.pointerId) return;
  const source = document.querySelector(`.flow-step[data-node-id="${CSS.escape(linking.sourceID)}"] .flow-port-out`)?.getBoundingClientRect();
  const spaceRect = $('flow-space').getBoundingClientRect();
  if (!source) return;
  const x1 = source.left + source.width / 2 - spaceRect.left;
  const y1 = source.top + source.height / 2 - spaceRect.top;
  const x2 = event.clientX - spaceRect.left;
  const y2 = event.clientY - spaceRect.top;
  $('flow-link-preview').setAttribute('d', flowPath(x1, y1, x2, y2));
}

function finishFlowConnection(event) {
  const linking = state.flowLink;
  if (!linking || event.pointerId !== linking.pointerId) return;
  const target = document.elementFromPoint(event.clientX, event.clientY)?.closest('.flow-port-in');
  $('flow-link-preview').setAttribute('d', '');
  state.flowLink = null;
  if (target?.dataset.nodeId) addFlowEdge(linking.sourceID, target.dataset.nodeId);
}

// flowTriggerTokens: wat de ALS-kaarten van deze Flow aan tokens aanbieden —
// voor de hintregel én de picker. Op naam ontdubbeld: twee ALS-kaarten die
// allebei "device" dragen zijn voor een veld één keuze.
function flowTriggerTokens() {
  const flow = state.editingFlow;
  if (!flow) return [];
  const seen = new Set();
  return flow.nodes
    .filter(flowNode => flowNodeKind(flowNode) === 'trigger')
    .flatMap(flowNode => cardForStep(flowNode.step)?.tokens || [])
    .filter(token => token?.name && !seen.has(token.name) && seen.add(token.name));
}

// withTokenPicker zet een kiezer naast een tekstveld: een token uit de lijst
// prikken in plaats van {{naam}} blind typen (todo §3). De lijst wordt bij het
// OPENEN bepaald, dus een ALS-kaart erbij of eraf is meteen zichtbaar zonder
// hertekenen; invoegen gebeurt op de cursorpositie en vuurt het gewone
// input-event, zodat step.args via de bestaande listener meeloopt.
function withTokenPicker(label, input) {
  const row = node('div', 'flow-input-row');
  const button = node('button', 'compact-icon-button icon-button flow-token-button');
  button.type = 'button';
  button.title = 'Triggerwaarde invoegen';
  button.append(materialIcon('data_object'));
  const list = node('div', 'suggestions hidden');
  const close = () => { list.classList.add('hidden'); list.replaceChildren(); };
  button.addEventListener('click', () => {
    if (!list.classList.contains('hidden')) { close(); return; }
    list.replaceChildren();
    const tokens = flowTriggerTokens();
    if (!tokens.length) list.append(node('p', 'empty', 'Nog geen ALS-kaart die waarden aanbiedt.'));
    for (const token of tokens) {
      const item = node('button', 'suggestion'); item.type = 'button';
      const text = node('span', 'suggestion-text');
      text.append(node('strong', '', localized(token.title) || token.name));
      text.append(node('small', '', `{{${token.name}}}`));
      item.append(text);
      // mousedown, niet click: de blur van de knop sluit de lijst anders vóór
      // de click aankomt (zelfde afweging als bij de autocomplete-suggesties).
      item.addEventListener('mousedown', event => {
        event.preventDefault();
        const at = input.selectionStart ?? input.value.length;
        input.setRangeText(`{{${token.name}}}`, at, input.selectionEnd ?? at, 'end');
        input.dispatchEvent(new Event('input'));
        input.focus();
        close();
      });
      list.append(item);
    }
    list.classList.remove('hidden');
  });
  button.addEventListener('blur', () => setTimeout(close, 120));
  row.append(input, button);
  label.append(row, list);
  return label;
}

function renderFlowArgument(step, card, argument, redraw = () => {}) {
  const label = node('label', 'flow-argument', argumentLabel(argument));
  let input;
  const value = step.args?.[argument.name];
  if (argument.type === 'capability') {
    const device = deviceForStep(step, card);
    input = node('select');
    if (!device) {
      const empty = node('option', '', 'Kies eerst een apparaat'); empty.value = '';
      input.append(empty); input.disabled = true;
      label.append(input); return label;
    }
    if (argument.optional) {
      const any = node('option', '', 'Elke waarde'); any.value = ''; input.append(any);
    }
    for (const capability of device.capabilities || []) {
      const option = node('option', '', capabilityTitle(device, capability));
      option.value = capability; input.append(option);
    }
    input.value = typeof value === 'string' ? value : '';
    if (!input.value && !argument.optional && device.capabilities?.length) {
      input.value = device.capabilities[0];
      step.args[argument.name] = input.value;
    }
    input.addEventListener('change', () => { step.args[argument.name] = input.value; redraw(); });
    label.append(input); return label;
  } else if (argument.type === 'capability-value') {
    const device = deviceForStep(step, card);
    // A derived card names its capability itself; the generic cards keep it
    // in the step.
    const definition = capabilityDefinition(device, card.capability || step.args?.capability);
    if (!definition) {
      input = node('select');
      const empty = node('option', '', 'Kies eerst een waarde'); empty.value = '';
      input.append(empty); input.disabled = true;
      label.append(input); return label;
    }
    const choices = capabilityChoices(definition);
    if (choices) {
      input = node('select');
      for (const choice of choices) {
        const option = node('option', '', choice.title); option.value = String(choice.id); input.append(option);
      }
      input.value = value === undefined || value === null ? '' : String(value);
      if (!input.value) { input.value = String(choices[0].id); step.args[argument.name] = choices[0].id; }
      input.addEventListener('change', () => { step.args[argument.name] = capabilityChoiceValue(input.value, definition); });
      label.append(input); return label;
    }
    input = node('input');
    input.type = definition.type === 'number' ? 'number' : 'text';
    if (definition.min !== undefined) input.min = definition.min;
    if (definition.max !== undefined) input.max = definition.max;
    if (definition.step !== undefined) input.step = definition.step;
    input.value = value === undefined || value === null ? '' : String(value);
    input.placeholder = definition.units ? `waarde in ${definition.units}` : 'waarde of {{token}}';
    input.addEventListener('input', () => { step.args[argument.name] = capabilityChoiceValue(input.value, definition); });
    if (input.type === 'text') return withTokenPicker(label, input);
    label.append(input); return label;
  } else if (argument.type === 'device') {
    input = node('select');
    const empty = node('option', '', 'Kies apparaat…'); empty.value = ''; input.append(empty);
    const manufacturers = new Map();
    for (const device of devicesForCard(card)) {
      const selectedGroup = state.deviceGroups.find(group => group.id === device.groupId);
      const groupName = selectedGroup ? groupPath(selectedGroup.id) : device.manufacturer || state.apps.find(app => app.id === device.appId)?.name || device.appId || 'Overig';
      if (!manufacturers.has(groupName)) manufacturers.set(groupName, []);
      manufacturers.get(groupName).push(device);
    }
    for (const [manufacturer, devices] of [...manufacturers].sort(([left], [right]) => left.localeCompare(right, 'nl'))) {
      const group = document.createElement('optgroup'); group.label = manufacturer;
      for (const device of devices.sort((left, right) => left.name.localeCompare(right.name, 'nl'))) {
        // De app erbij, want twee apparaten mogen dezelfde naam dragen: een
        // "Woonkamer" die een lamp is en een "Woonkamer" die een camera is
        // staan anders als twee identieke regels in de lijst.
        const app = localized(state.apps.find(candidate => candidate.id === device.appId)?.name);
        const option = node('option', '', app ? `${device.name} — ${app}` : device.name);
        option.value = device.id; group.append(option);
      }
      input.append(group);
    }
    input.value = value?.$device || '';
    input.addEventListener('change', () => {
      step.args[argument.name] = { $device: input.value };
      // A different device exposes different capabilities, so anything
      // derived from the old one has to go.
      if (step.args.capability !== undefined) { step.args.capability = ''; step.args.value = ''; }
      redraw();
    });
  } else if (argument.type === 'dropdown') {
    input = node('select');
    for (const choice of argument.values || []) {
      const option = node('option', '', localized(choice.title) || choice.id); option.value = choice.id; input.append(option);
    }
    input.value = value ?? '';
    input.addEventListener('change', () => { step.args[argument.name] = input.value; });
  } else if (argument.type === 'autocomplete') {
    // Een echte keuzelijst en geen <datalist>. Die laatste ziet eruit als een
    // gewoon tekstveld, toont alleen de titel, en koppelt de keuze terug op die
    // titel -- dus twee nummers die "Blue Monday" heten zijn niet uit elkaar te
    // houden en de artiest is nergens te zien. Kiezen moet je kunnen zien.
    input = node('input'); input.type = 'text'; input.value = autocompleteLabel(value);
    input.placeholder = localized(argument.placeholder) || 'Typ om te zoeken…';
    input.autocomplete = 'off';
    const list = node('div', 'suggestions hidden');
    // Wat er gekozen is, onder het veld. Zonder dit is er geen verschil te zien
    // tussen "ik heb iets getypt" en "ik heb iets gekozen" -- en dat verschil is
    // wezenlijk: getypte tekst is geen nummer, en de kaart kan er niets mee.
    const chosen = node('div', 'chosen');
    let timer; let latest = 0;

    const showChosen = value => {
      chosen.replaceChildren();
      if (value && typeof value === 'object') {
        chosen.className = 'chosen';
        if (value.image) {
          const cover = node('img'); cover.src = value.image; cover.alt = ''; chosen.append(cover);
        }
        const text = node('span', 'suggestion-text');
        text.append(node('strong', '', autocompleteLabel(value)));
        if (value.description) text.append(node('small', '', value.description));
        chosen.append(text);
        return;
      }
      chosen.className = 'chosen unchosen';
      chosen.append(node('small', '', input.value.trim()
        ? 'Nog niets gekozen — klik een resultaat aan.'
        : 'Typ om te zoeken.'));
    };

    const close = () => { list.classList.add('hidden'); list.replaceChildren(); };
    const choose = choice => {
      step.args[argument.name] = choice;
      input.value = autocompleteLabel(choice);
      showChosen(choice);
      close();
    };
    const refresh = async () => {
      const mine = ++latest;
      try {
        const values = await api('/api/stulp/flow/autocomplete', {
          method: 'POST', body: JSON.stringify({ appId: card.appId, cardId: card.id, cardType: card.type, argument: argument.name, query: input.value, args: step.args }),
        });
        // Een trager antwoord op een oudere vraag mag een nieuwer niet
        // overschrijven -- typen gaat sneller dan een cloud antwoordt.
        if (mine !== latest) return;
        const choices = (Array.isArray(values) ? values : []).filter(choice => autocompleteLabel(choice));
        list.replaceChildren(...choices.map(choice => {
          const item = node('button', 'suggestion'); item.type = 'button';
          if (choice.image) {
            const cover = node('img'); cover.src = choice.image; cover.alt = ''; cover.loading = 'lazy';
            item.append(cover);
          }
          const text = node('span', 'suggestion-text');
          text.append(node('strong', '', autocompleteLabel(choice)));
          if (choice.description) text.append(node('small', '', choice.description));
          item.append(text);
          // mousedown en niet click: blur van het invoerveld komt eerder dan
          // click, en zou de lijst dan al gesloten hebben.
          item.addEventListener('mousedown', event => { event.preventDefault(); choose(choice); });
          return item;
        }));
        if (!choices.length) list.append(node('p', 'empty', 'Niets gevonden.'));
        list.classList.remove('hidden');
      } catch (error) {
        // De kaart blijft bewerkbaar als een integratie niet antwoordt, maar
        // zwijgen zou lijken alsof er niets te vinden is.
        list.replaceChildren(node('p', 'empty bad', error.message || String(error)));
        list.classList.remove('hidden');
      }
    };

    input.addEventListener('focus', refresh);
    input.addEventListener('input', () => {
      // Typen maakt een eerdere keuze ongedaan. Hem laten staan zou de kaart
      // iets anders laten doen dan er in het veld staat.
      step.args[argument.name] = input.value;
      showChosen(null);
      clearTimeout(timer); timer = setTimeout(refresh, 180);
    });
    input.addEventListener('keydown', event => {
      if (event.key === 'Escape') { close(); return; }
      if (event.key === 'ArrowDown') { event.preventDefault(); list.querySelector('.suggestion')?.focus(); }
    });
    input.addEventListener('blur', () => setTimeout(close, 120));
    showChosen(value);
    label.append(input, list, chosen); return label;
  } else if (argument.type === 'checkbox') {
    input = node('input'); input.type = 'checkbox'; input.checked = Boolean(value);
    input.addEventListener('change', () => { step.args[argument.name] = input.checked; });
  } else {
	input = node('input'); input.type = argument.type === 'number' || argument.type === 'range' ? 'number' : argument.type === 'time' ? 'time' : 'text'; input.value = value ?? '';
    if (argument.min !== undefined) input.min = argument.min;
    if (argument.max !== undefined) input.max = argument.max;
    if (argument.step !== undefined) input.step = argument.step;
    input.placeholder = argument.type === 'json' ? 'true, 42, "tekst" of {{token}}' : '';
    input.addEventListener('input', () => { step.args[argument.name] = flowArgumentValue(input.value, argument.type); });
    if (input.type === 'text') return withTokenPicker(label, input);
  }
  label.append(input); return label;
}

// Many app manifests leave the device argument untitled, which would show
// the raw field name. Fall back to what the field is rather than what it is
// called in the manifest.
function argumentLabel(argument) {
  const title = localized(argument.title);
  // De eenheid bij het veld, zodat je ziet waarin je typt. Hij komt van de
  // server en is dus de eenheid van dit huis.
  const units = localized(argument.units);
  if (title) return units ? `${title} (${units})` : title;
  if (argument.type === 'device') return 'Apparaat';
  if (argument.type === 'capability') return 'Waarde';
  return argument.name;
}

function autocompleteLabel(value) {
  if (typeof value === 'string') return value;
  return value?.name || localized(value?.title) || value?.label || value?.id || '';
}

function flowArgumentValue(value, type) {
  if (type === 'number' || type === 'range') return value === '' ? null : Number(value);
  if (type === 'json' && !value.includes('{{')) {
    try { return JSON.parse(value); } catch (_) { return value; }
  }
  return value;
}

// The server resolves each card's device filter, so the browser does not
// re-implement the filter grammar (which has alternatives, multiple
// conditions and an object form).
function devicesForCard(card) {
  if (!Array.isArray(card?.deviceIds)) return state.devices;
  const allowed = new Set(card.deviceIds);
  return state.devices.filter(device => allowed.has(device.id));
}

function deviceForStep(step, card) {
  const argument = (card?.args || []).find(entry => entry.type === 'device');
  const id = argument ? step.args?.[argument.name]?.$device : null;
  return id ? state.devices.find(device => device.id === id) : null;
}

function capabilityDefinition(device, capability) {
  if (!device || !capability) return null;
  return device.capabilitiesObj?.[capability] || null;
}

function capabilityTitle(device, capability) {
  const definition = capabilityDefinition(device, capability);
  const title = localized(definition?.title);
  return title && title !== capability ? `${title} (${capability})` : capability;
}

// capabilityChoices returns a closed set of options when the capability has
// one, so the editor can offer a list instead of a blank field.
function capabilityChoices(definition) {
  if (Array.isArray(definition.values) && definition.values.length) {
    return definition.values.map(choice => ({
      id: choice.id ?? choice, title: localized(choice.title) || String(choice.id ?? choice),
    }));
  }
  if (definition.type === 'boolean') return [{ id: true, title: 'Aan' }, { id: false, title: 'Uit' }];
  return null;
}

function capabilityChoiceValue(raw, definition) {
  if (typeof raw === 'string' && raw.includes('{{')) return raw;
  if (definition.type === 'boolean') return raw === 'true' || raw === true;
  if (definition.type === 'number') return raw === '' ? null : Number(raw);
  return raw;
}

// stepFromCard fills in every argument it can work out, so a freshly added
// card is usable rather than a row of blanks. preferredDevice is the device
// the card was picked from in the device-first browser.
function stepFromCard(card, preferredDevice = null) {
  const args = {};
  const candidates = devicesForCard(card);
  const device = candidates.find(entry => entry.id === preferredDevice?.id) || candidates[0] || null;
  for (const argument of card.args || []) {
    if (argument.type === 'device') args[argument.name] = { $device: device?.id || '' };
    else if (argument.type === 'checkbox') args[argument.name] = Boolean(argument.value);
    else if (argument.type === 'dropdown') args[argument.name] = argument.value ?? argument.values?.[0]?.id ?? '';
    else if (argument.type === 'capability') args[argument.name] = argument.optional ? '' : device?.capabilities?.[0] || '';
    else if (argument.type === 'capability-value') args[argument.name] = '';
    else args[argument.name] = argument.value ?? '';
  }
  const step = { appId: card.appId, cardId: card.id, cardType: card.type, args };
  // The value widget follows the capability, so seed it the same way the
  // editor would once both are known.
  const definition = capabilityDefinition(device, card.capability || args.capability);
  if (definition) {
    const choices = capabilityChoices(definition);
    if (choices) args.value = choices[0].id;
  }
  return step;
}

function cardKey(value) { return `${value?.appId || ''}|${value?.type || value?.cardType || ''}|${value?.id || value?.cardId || ''}`; }
// flowStepSentence maakt van een kaart de regel die in de Flow staat.
//
// Met titleFormatted en de gekozen waarden erin gevuld leest een Flow als een
// zin: "Windkracht komt boven 6 Bft" in plaats van "Windkracht komt boven een
// grens" met dat getal ergens in een veld eronder. Een argument dat nog niet
// gekozen is blijft als [[naam]] staan -- dan zie je meteen wat er mist.
function flowStepSentence(card, step) {
  if (!card) return step.cardId;
  if (!card.titleFormatted) return card.title || step.cardId;
  // !{{zo|anders}} is de omkering: een EN-kaart met "Niet" aangevinkt leest de
  // tweede helft. Zonder dit staat die krul letterlijk in de Flow.
  let sentence = card.titleFormatted.replace(/!\{\{([^|}]*)\|([^}]*)\}\}/g,
    (whole, plain, inverted) => (step.inverted ? inverted : plain));
  return sentence.replace(/\[\[(\w+)\]\]/g, (whole, name) => {
    const argument = (card.args || []).find(item => item.name === name);
    const value = step.args?.[name];
    if (value === undefined || value === null || value === '') return whole;
    // Een keuzelijst toont zijn label en niet zijn id: "6 Bft", niet "6".
    if (Array.isArray(argument?.values)) {
      const chosen = argument.values.find(item => String(item.id) === String(value));
      if (chosen) return localized(chosen.label || chosen.title) || String(value);
    }
    // Een autocomplete bewaart het hele item; daarvan is de naam wat je leest.
    if (value && typeof value === 'object') return autocompleteLabel(value) || whole;
    // Een getal met een eenheid leest met die eenheid erbij, en die komt van de
    // server: in een huis dat Fahrenheit leest staat er "boven 68 °F". Daarom
    // staat de eenheid niet in de zin zelf in app.json — dan zou er "68 °F °C"
    // komen te staan.
    if (argument?.units && Number.isFinite(Number(value))) return formatValue(Number(value), argument.units);
    return String(value);
  });
}

function cardForStep(step) {
  for (const group of Object.values(state.flowCards)) {
    const card = group.find(candidate => cardKey(candidate) === cardKey(step));
    if (card) return card;
  }
  return null;
}

function flowNodeKind(flowNode) {
  return flowNode?.step?.cardType === 'condition' ? 'condition' : flowNode?.step?.cardType === 'action' ? 'action' : 'trigger';
}

function flowNodeCounts(flow) {
  const counts = { trigger: 0, condition: 0, action: 0 };
  for (const flowNode of flow.nodes || []) counts[flowNodeKind(flowNode)]++;
  return counts;
}

function flowID() {
  return crypto.randomUUID ? crypto.randomUUID() : `flow-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function removeFlowNode(id) {
  state.editingFlow.nodes = state.editingFlow.nodes.filter(flowNode => flowNode.id !== id);
  state.editingFlow.edges = state.editingFlow.edges.filter(edge => edge.from !== id && edge.to !== id);
  renderFlowEditor();
}

function openFlowTypePicker(point = null) {
  state.flowAddPoint = point;
  $('flow-type-dialog').showModal();
}

function openFlowCardPicker(kind) {
  const cards = usableFlowCards(kind);
  if (!cards.length) return toast(`Geen beschikbare ${kind === 'trigger' ? 'startpunten' : kind === 'condition' ? 'voorwaarden' : 'eindpunten'}.`, true);
  state.flowPickerKind = kind;
  state.flowPickerDevice = null;
  $('flow-card-title').textContent = kind === 'trigger' ? 'ALS toevoegen' : kind === 'condition' ? 'EN toevoegen' : 'DAN toevoegen';
  $('flow-card-search').value = '';
  renderFlowCardChoices();
  $('flow-card-dialog').showModal();
  $('flow-card-search').focus();
}

// A card is only worth offering when its app can run it and, if it is
// device-scoped, at least one device matches. Offering "Garage door opened"
// without a garage door is what made this list unusable.
function usableFlowCards(kind) {
  return (state.flowCards[`${kind}s`] || [])
    .filter(card => card.available && (card.scope !== 'device' || (card.deviceIds || []).length));
}

// The catch-all cards can express anything, which is exactly why they are
// the least helpful place to start. Put what the device concretely does
// first and leave the generic ones at the bottom.
const genericFlowCards = new Set(['device_capability_changed', 'device_capability_equals', 'set_device_capability', 'matter_event']);

function flowCardRank(card) {
  if (genericFlowCards.has(card.id)) return 2;
  if (card.capability) return 1;
  return 0;
}

function flowCardsForDevice(kind, deviceID) {
  return usableFlowCards(kind)
    .filter(card => (card.deviceIds || []).includes(deviceID))
    .sort((left, right) => flowCardRank(left) - flowCardRank(right));
}

function flowCardMatchesQuery(card, query) {
  return !query || `${card.appName} ${card.title} ${card.hint || ''}`.toLocaleLowerCase().includes(query);
}

function renderFlowCardChoices() {
  const kind = state.flowPickerKind;
  const query = $('flow-card-search').value.trim().toLocaleLowerCase();
  const target = $('flow-card-choices');
  target.replaceChildren();
  if (state.flowPickerDevice) return renderFlowDeviceCards(kind, query, target);
  if (query) return renderFlowCardList(kind, usableFlowCards(kind).filter(card => flowCardMatchesQuery(card, query)), target, true);
  renderFlowDeviceBrowser(kind, target);
}

// The first screen is the home, not the software: groups, then devices, then
// what that device can actually do.
function renderFlowDeviceBrowser(kind, target) {
  const cards = usableFlowCards(kind);
  const devices = state.devices
    .map(device => ({ device, cards: cards.filter(card => (card.deviceIds || []).includes(device.id)) }))
    .filter(entry => entry.cards.length);

  const groups = new Map();
  const ungrouped = [];
  for (const entry of devices) {
    const group = state.deviceGroups.find(candidate => candidate.id === entry.device.groupId);
    if (!group) {
      ungrouped.push(entry);
      continue;
    }
    if (!groups.has(group.id)) groups.set(group.id, []);
    groups.get(group.id).push(entry);
  }
  const appendGroup = (groupName, entries) => {
    const section = node('section', 'flow-choice-group');
    section.append(node('h3', '', groupName));
    for (const entry of entries.sort((left, right) => left.device.name.localeCompare(right.device.name, 'nl'))) {
      const button = node('button', 'flow-card-choice');
      button.type = 'button';
      const icon = node('span', `flow-choice-icon ${kind}-choice-icon`);
      icon.append(materialIcon(deviceClassSymbol(entry.device.class)));
      const copy = node('span', 'flow-choice-copy');
      copy.append(node('strong', '', entry.device.name));
      copy.append(node('small', '', `${entry.cards.length} ${entry.cards.length === 1 ? 'mogelijkheid' : 'mogelijkheden'}`));
      button.append(icon, copy, materialIcon('chevron_right', 'flow-choice-add'));
      button.addEventListener('click', () => {
        state.flowPickerDevice = entry.device;
        renderFlowCardChoices();
      });
      section.append(button);
    }
    target.append(section);
  };
  for (const group of deviceGroupsInDisplayOrder()) {
    const entries = groups.get(group.id);
    if (entries?.length) appendGroup(groupPath(group.id), entries);
  }
  if (ungrouped.length) appendGroup('Zonder groep', ungrouped);

  // Cards that belong to the home rather than to one device: time, sun,
  // notifications, and any app-wide card.
  const general = cards.filter(card => card.scope !== 'device');
  if (general.length) {
    const section = node('section', 'flow-choice-group');
    section.append(node('h3', '', 'Niet aan één apparaat gebonden'));
    for (const card of general) section.append(flowCardButton(kind, card, card.appName));
    target.append(section);
  }
  if (!devices.length && !general.length) {
    target.append(node('p', 'empty flow-choice-empty', 'Nog geen apparaten met mogelijkheden.'));
  }
}

function renderFlowDeviceCards(kind, query, target) {
  const device = state.flowPickerDevice;
  const back = node('button', 'flow-choice-back');
  back.type = 'button';
  back.append(materialIcon('arrow_back'), document.createTextNode('Alle apparaten'));
  back.addEventListener('click', () => { state.flowPickerDevice = null; renderFlowCardChoices(); });
  target.append(back);

  const cards = flowCardsForDevice(kind, device.id).filter(card => flowCardMatchesQuery(card, query));
  const section = node('section', 'flow-choice-group');
  section.append(node('h3', '', `${device.name} kan`));
  for (const card of cards) section.append(flowCardButton(kind, card, card.appName, device));
  target.append(section);
  if (!cards.length) target.append(node('p', 'empty flow-choice-empty', 'Geen mogelijkheden gevonden.'));
}

function renderFlowCardList(kind, cards, target, showAppName) {
  const groups = new Map();
  for (const card of cards) {
    if (!groups.has(card.appName)) groups.set(card.appName, []);
    groups.get(card.appName).push(card);
  }
  for (const [appName, appCards] of groups) {
    const section = node('section', 'flow-choice-group');
    section.append(node('h3', '', appName));
    for (const card of appCards) section.append(flowCardButton(kind, card, showAppName ? appName : ''));
    target.append(section);
  }
  if (!cards.length) target.append(node('p', 'empty flow-choice-empty', 'Geen kaarten gevonden.'));
}

function flowCardButton(kind, card, _iconSource, device = null) {
  const button = node('button', 'flow-card-choice');
  button.type = 'button';
  const icon = node('span', `flow-choice-icon ${kind}-choice-icon`);
  icon.append(materialIcon(device ? deviceClassSymbol(device.class) : kind === 'trigger' ? 'event' : kind === 'condition' ? 'rule' : 'bolt'));
  const copy = node('span', 'flow-choice-copy');
  copy.append(node('strong', '', card.title || card.id));
  if (card.hint) copy.append(node('small', '', card.hint));
  button.append(icon, copy, materialIcon('add', 'flow-choice-add'));
  button.addEventListener('click', () => addFlowCard(kind, card, device));
  return button;
}

function addFlowCard(kind, card, device = null) {
  const canvas = $('flow-canvas');
  const cascade = (state.editingFlow.nodes.length % 7) * 24;
  const point = state.flowAddPoint || {
    x: canvas.scrollLeft + Math.max(40, (canvas.clientWidth - 288) / 2) + cascade,
    y: canvas.scrollTop + Math.max(40, (canvas.clientHeight - 160) / 2) + cascade,
  };
  const flowNode = { id: flowID(), x: Math.round(point.x), y: Math.round(point.y), step: stepFromCard(card, device) };
  state.editingFlow.nodes.push(flowNode);
  $('flow-card-dialog').close();
  state.flowPickerKind = null;
  state.flowPickerDevice = null;
  state.flowAddPoint = null;
  renderFlowEditor();
}

function watchFlowLayout() {
  flowResizeObserver.disconnect();
  const canvas = $('flow-canvas');
  if (!canvas) return;
  flowResizeObserver.observe(canvas);
  flowResizeObserver.observe($('flow-space'));
  canvas.querySelectorAll('.flow-step').forEach(card => flowResizeObserver.observe(card));
}

function scheduleFlowConnections() {
  cancelAnimationFrame(flowDrawFrame);
  flowDrawFrame = requestAnimationFrame(drawFlowConnections);
}

function drawFlowConnections() {
  const canvas = $('flow-canvas');
  const svg = $('flow-links');
  const paths = $('flow-link-paths');
  if (!canvas || !svg || !paths || !$('flow-dialog').open) return;
  paths.replaceChildren();
  const space = $('flow-space');
  const width = space.clientWidth;
  const height = space.clientHeight;
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('width', width);
  svg.setAttribute('height', height);
  const spaceRect = space.getBoundingClientRect();
  const nodes = new Map(state.editingFlow.nodes.map(flowNode => [flowNode.id, flowNode]));
  for (const edge of state.editingFlow.edges) {
    const sourceCard = document.querySelector(`.flow-step[data-node-id="${CSS.escape(edge.from)}"]`);
    const destinationCard = document.querySelector(`.flow-step[data-node-id="${CSS.escape(edge.to)}"]`);
    const source = sourceCard?.querySelector('.flow-port-out')?.getBoundingClientRect();
    const destination = destinationCard?.querySelector('.flow-port-in')?.getBoundingClientRect();
    if (!source || !destination) continue;
    const x1 = source.left + source.width / 2 - spaceRect.left;
    const y1 = source.top + source.height / 2 - spaceRect.top;
    const x2 = destination.left + destination.width / 2 - spaceRect.left;
    const y2 = destination.top + destination.height / 2 - spaceRect.top;
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', flowPath(x1, y1, x2, y2));
    path.setAttribute('class', `flow-link flow-link-${flowNodeKind(nodes.get(edge.to))}`);
    path.setAttribute('marker-end', 'url(#flow-arrow)');
    paths.append(path);
    const hit = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    hit.setAttribute('d', flowPath(x1, y1, x2, y2));
    hit.setAttribute('class', 'flow-link-hit');
    hit.addEventListener('click', () => {
      state.editingFlow.edges = state.editingFlow.edges.filter(candidate => candidate.id !== edge.id);
      drawFlowConnections();
    });
    paths.append(hit);
  }
}

function flowPath(x1, y1, x2, y2) {
  const direction = x2 >= x1 ? 1 : -1;
  const bend = Math.max(55, Math.abs(x2 - x1) * .42);
  return `M ${x1} ${y1} C ${x1 + direction * bend} ${y1}, ${x2 - direction * bend} ${y2}, ${x2} ${y2}`;
}

function addFlowEdge(from, to) {
  if (from === to) return toast('Een kaart kan niet met zichzelf verbinden.', true);
  const target = state.editingFlow.nodes.find(flowNode => flowNode.id === to);
  if (!target || flowNodeKind(target) === 'trigger') return toast('Een ALS-kaart is altijd een startpunt.', true);
  if (state.editingFlow.edges.some(edge => edge.from === from && edge.to === to)) return;
  if (flowConnectionCreatesCycle(from, to)) return toast('Deze verbinding zou een rondje maken.', true);
  state.editingFlow.edges.push({ id: flowID(), from, to });
  renderFlowEditor();
}

function flowConnectionCreatesCycle(from, to) {
  const adjacency = new Map();
  for (const edge of [...state.editingFlow.edges, { from, to }]) {
    if (!adjacency.has(edge.from)) adjacency.set(edge.from, []);
    adjacency.get(edge.from).push(edge.to);
  }
  const seen = new Set();
  const reaches = id => {
    if (id === from) return true;
    if (seen.has(id)) return false;
    seen.add(id);
    return (adjacency.get(id) || []).some(reaches);
  };
  return reaches(to);
}

async function saveFlow(event) {
  event.preventDefault();
  const flow = state.editingFlow;
  flow.name = $('flow-name').value.trim(); flow.enabled = $('flow-enabled').checked;
  try {
    const path = flow.id ? `/api/manager/flow/flow/${encode(flow.id)}` : '/api/manager/flow/flow';
    await api(path, { method: flow.id ? 'PUT' : 'POST', body: JSON.stringify(flow) });
    $('flow-dialog').close(); await load(); toast('Flow opgeslagen');
  } catch (error) { toast(error.message, true); }
}
async function testEditedFlow() {
  const flow = state.editingFlow;
  if (!flow?.id) return toast('Sla deze Flow eerst op.', true);
  if (!$('flow-form').reportValidity()) return;
  flow.name = $('flow-name').value.trim(); flow.enabled = $('flow-enabled').checked;
  const button = $('flow-test'); button.disabled = true;
  try {
    const updated = await api(`/api/manager/flow/flow/${encode(flow.id)}`, { method: 'PUT', body: JSON.stringify(flow) });
    state.editingFlow = JSON.parse(JSON.stringify(updated));
    const result = await api(`/api/stulp/flows/${encode(flow.id)}/run`, { method: 'POST' });
    await load();
    renderFlowEditor();
    toast(result.stopped ? 'Test gestopt: een voorwaarde was niet waar' : 'Flow succesvol uitgevoerd');
  } catch (error) { await load(); toast(error.message, true); }
  finally { button.disabled = false; }
}
async function toggleFlow(flow) {
  try {
    await api(`/api/manager/flow/flow/${encode(flow.id)}/enabled`, { method: 'PUT', body: JSON.stringify({ enabled: !flow.enabled }) });
    await load();
  } catch (error) { toast(error.message, true); }
}
async function runFlow(flow) {
  try {
    const result = await api(`/api/stulp/flows/${encode(flow.id)}/run`, { method: 'POST' });
    await load(); toast(result.stopped ? 'Flow getest: voorwaarde was niet waar' : 'Flow succesvol uitgevoerd');
  } catch (error) { await load(); toast(error.message, true); }
}
async function deleteFlow(flow) {
  if (!confirm(`Flow “${flow.name}” verwijderen?`)) return;
  try { await api(`/api/manager/flow/flow/${encode(flow.id)}`, { method: 'DELETE' }); await load(); }
  catch (error) { toast(error.message, true); }
}

function isSceneDevice(device) { return device.appId === nativeSceneAppID; }

function sceneForDevice(device) {
  if (!isSceneDevice(device)) return null;
  return state.scenes.find(scene => scene.id === device.data?.sceneId) || null;
}

function renderSceneConfiguration(form, device) {
  const scene = sceneForDevice(device);
  form.append(node('h3', 'device-config-section', 'Scene-instellingen'));
  if (!scene) {
    form.append(node('p', 'app-error', 'De configuratie achter dit scene-apparaat ontbreekt.'));
    return;
  }

  const deviceCount = new Set((scene.states || []).map(wanted => wanted.deviceId)).size;
  form.append(node('p', 'settings-hint scene-config-explanation',
    `Bij aan worden ${scene.states.length} ${scene.states.length === 1 ? 'stand' : 'standen'} op ${deviceCount} ${deviceCount === 1 ? 'apparaat' : 'apparaten'} gezet. Bij uit herstelt Stulp de standen die het vlak voor aan heeft onthouden.`));

  const preview = node('div', 'scene-config-preview');
  for (const wanted of (scene.states || []).slice(0, 8)) {
    preview.append(node('div', 'scene-config-state', sceneStateDescription(wanted)));
  }
  if (scene.states.length > 8) preview.append(node('div', 'scene-config-more', `en nog ${scene.states.length - 8}…`));
  form.append(preview);

  const active = Boolean(device.capabilitiesObj?.onoff?.value ?? scene.active);
  const edit = actionButton('Standen configureren', () => openScene(scene));
  edit.classList.add('button-with-icon');
  edit.prepend(materialIcon('tune'));
  edit.disabled = active;
  if (active) edit.title = 'Zet het scene-apparaat eerst uit; Stulp bewaart de vorige standen zolang het aan staat.';
  const actions = node('div', 'scene-config-actions');
  actions.append(edit);
  if (active) actions.append(node('span', 'settings-hint', 'Eerst uitzetten om de herstelstanden niet te verliezen.'));
  form.append(actions);
}

function renderDeviceConfiguration(device, driver) {
  const isScene = isSceneDevice(device);
  const scene = sceneForDevice(device);
  const sceneActive = Boolean(device.capabilitiesObj?.onoff?.value ?? scene?.active);
  $('device-settings-status').textContent = isScene
    ? 'Ingebouwd scene-apparaat · aan onthoudt de vorige standen'
    : `Hardware: ${device.hardwareName || device.name}`;
  const form = $('device-settings');
  form.replaceChildren();

  const nameField = node('label', 'settings-field');
  nameField.append(node('span', 'settings-label', 'Naam'));
  nameField.append(node('small', 'settings-hint', 'De naam waaronder dit apparaat in Stulp en Flows staat.'));
  const nameInput = node('input'); nameInput.id = 'device-config-name'; nameInput.type = 'text'; nameInput.maxLength = 100; nameInput.required = true; nameInput.value = device.name;
  if (isScene && sceneActive) {
    nameInput.disabled = true;
    nameField.querySelector('.settings-hint').textContent = 'Zet de scene eerst uit om de naam of standen te wijzigen.';
  }
  nameField.append(nameInput); form.append(nameField);

  const groupField = node('label', 'settings-field');
  groupField.append(node('span', 'settings-label', 'Groep'));
  groupField.append(node('small', 'settings-hint', 'Kies de plek van dit apparaat in de groepsboom.'));
  const groupSelect = node('select'); groupSelect.id = 'device-config-group';
  const ungrouped = node('option', '', 'Overig'); ungrouped.value = ''; groupSelect.append(ungrouped);
  appendGroupOptions(groupSelect, device.groupId || ''); groupSelect.value = device.groupId || '';
  groupField.append(groupSelect); form.append(groupField);

  const driverSettings = driver?.settings || [];
  const settings = (driverSettings.length ? driverSettings : device.settingsSpec || []).filter(setting => setting.id);
  if (settings.length) form.append(node('h3', 'device-config-section', 'Apparaatinstellingen'));
  for (const setting of settings) {
    const label = node('label', `settings-field ${setting.type === 'checkbox' ? 'checkbox-setting' : ''}`);
    label.append(node('span', 'settings-label', localized(setting.label) || setting.id));
    if (setting.hint) label.append(node('small', 'settings-hint', localized(setting.hint)));
    let input;
    if (setting.type === 'checkbox') {
      input = node('input'); input.type = 'checkbox'; input.checked = Boolean(device.settings?.[setting.id] ?? setting.value);
    } else if (setting.type === 'dropdown') {
      input = node('select');
      for (const value of setting.values || []) {
        const option = node('option', '', localized(value.label) || value.id); option.value = value.id; input.append(option);
      }
      input.value = device.settings?.[setting.id] ?? setting.value ?? '';
    } else {
      input = node('input'); input.type = setting.type === 'number' ? 'number' : 'text'; input.value = device.settings?.[setting.id] ?? setting.value ?? '';
      if (setting.attr) Object.assign(input, setting.attr);
      if (setting.placeholder) input.placeholder = localized(setting.placeholder);
    }
    input.dataset.setting = setting.id; input.dataset.type = setting.type;
    label.append(input); form.append(label);
  }
  if (isScene) renderSceneConfiguration(form, device);
  const actions = node('div', 'actions');
  const save = actionButton('Opslaan', () => {}, 'primary'); save.id = 'device-settings-save'; save.type = 'submit';
  actions.append(save);
  form.append(actions);
  form.onsubmit = event => {
	event.preventDefault();
	void saveDeviceConfiguration(device);
  };
}
async function saveDeviceConfiguration(device) {
  const nameInput = $('device-config-name');
  const name = nameInput.value.trim();
  if (!name) {
	nameInput.focus();
	return;
  }
  const patch = {};
  const groupID = $('device-config-group').value;
  const settingInputs = [...$('device-settings').querySelectorAll('[data-setting]')];
  settingInputs.forEach(input => {
    patch[input.dataset.setting] = input.dataset.type === 'checkbox' ? input.checked : input.dataset.type === 'number' ? Number(input.value) : input.value;
  });
  const button = $('device-settings-save');
  button.disabled = true;
  $('device-settings-status').textContent = 'Opslaan…';
  try {
	await renameDevice(device, name);
	await setDeviceGroup(device, groupID);
	if (settingInputs.length) {
	  await api(`/api/manager/devices/device/${encode(device.id)}/settings`, { method: 'PUT', body: JSON.stringify(patch) });
	}
    await load();
	const updated = state.devices.find(candidate => candidate.id === device.id) || device;
	updateDevicePopoverHeader(updated);
	renderDeviceOverview(updated);
	renderDeviceConfiguration(updated, state.drivers.find(item => item.id === updated.driverId));
	showDeviceTab('configuration');
	$('device-settings-status').textContent = isSceneDevice(updated)
	  ? 'Opgeslagen · ingebouwd scene-apparaat'
	  : `Opgeslagen · Hardware: ${updated.hardwareName || updated.name}`;
	toast('Configuratie opgeslagen');
  } catch (error) {
    $('device-settings-status').textContent = error.message;
    toast(error.message, true);
  } finally { button.disabled = false; }
}
async function deleteDevice(device) {
  if (!confirm(`${device.name} verwijderen?`)) return;
  try {
	await api(`/api/manager/devices/device/${encode(device.id)}`, { method: 'DELETE' });
	if (state.openDeviceID === device.id) $('device-popover').close();
	await load();
  }
  catch (error) { toast(error.message, true); }
}

function chooseDriver() {
  const content = $('pair-content');
  $('pair-title').textContent = 'Device toevoegen';
  $('pair-frame').classList.add('hidden');
  content.replaceChildren();
  const groups = node('div', 'plugin-groups');

  // Scenes zijn native omdat ze apparaten van meerdere, onderling geïsoleerde
  // apps moeten kunnen lezen en bedienen. Voor de gebruiker is dat detail niet
  // anders dan bij een plugin-driver: het is een apparaattype van Stulp en hoort
  // daarom hier, op dezelfde plek waar ieder ander apparaat wordt toegevoegd.
  const builtIn = node('section', 'plugin-group');
  const builtInTitle = node('div', 'plugin-title');
  builtInTitle.append(node('strong', '', 'Stulp'), node('small', 'muted', '1 apparaattype'));
  const builtInChoices = node('div', 'choices');
  const scene = node('button', 'choice');
  scene.append(node('strong', '', 'Scene'), node('small', 'muted', 'Virtueel aan/uit-apparaat'));
  scene.addEventListener('click', () => openScene());
  builtInChoices.append(scene); builtIn.append(builtInTitle, builtInChoices); groups.append(builtIn);

  for (const app of state.apps) {
    const drivers = state.drivers.filter(item => item.ownerUri === `stulp:app:${app.id}` && item.pair && item.ready);
    if (!drivers.length) continue;
    const group = node('section', 'plugin-group');
    const title = node('div', 'plugin-title');
    title.append(node('strong', '', app.name || app.id), node('small', 'muted', `${drivers.length} apparaat${drivers.length === 1 ? '' : 'typen'}`));
    group.append(title);
    const choices = node('div', 'choices');
    for (const driver of drivers) {
      const button = node('button', 'choice');
      button.append(node('strong', '', driver.name), node('small', 'muted', driver.class || driver.id));
      button.addEventListener('click', () => startPair(driver));
      choices.append(button);
    }
    group.append(choices); groups.append(group);
  }
  if (!groups.childElementCount) groups.append(node('p', 'empty', 'Geen pairbare apps of drivers beschikbaar.'));
  content.append(groups);
  $('pair-dialog').showModal();
}

async function startPair(driver) {
  const appId = driver.ownerUri.replace('stulp:app:', '');
  const driverId = driver.id.slice(driver.id.lastIndexOf(':') + 1);
  try {
    const session = await api('/api/stulp/pair', { method: 'POST', body: JSON.stringify({ appId, driverId }) });
    state.pair = { ...session, driver };
    $('pair-title').textContent = driver.name;
    const first = driver.pairViews?.[0];
    if (first?.id && driver.customPairViews?.includes(first.id)) showPairView(first.id);
    else {
      if (session.handlers.includes('validate')) {
        const result = await pairEmit('validate', null);
        if (result === 'nok' || result === false) throw new Error('Configureer de app eerst.');
      }
      // Het template van de driver bepaalt de eerste stap. Zonder een bekend
      // template is dat zoeken naar apparaten, want dat is wat vrijwel elke
      // driver doet.
      await showPairView(first?.template === 'push_subscription' ? 'push_subscription' : 'list_devices');
    }
  } catch (error) { toast(error.message, true); }
}
async function showPairView(view) {
  const pair = state.pair;
  if (!pair) return;
  if (pair.driver.customPairViews?.includes(view)) {
    $('pair-content').replaceChildren();
    $('pair-frame').classList.remove('hidden');
    $('pair-frame').src = `/app-ui/${encode(pair.appId)}/pair/${encode(pair.driverId)}/${encode(view)}.html?session=${encode(pair.id)}`;
    return;
  }
  $('pair-frame').classList.add('hidden');
  if (view === 'push_subscription') return showPushSubscriptionView();
  if (view === 'list_devices') {
    // Zoeken kan seconden duren -- een Modbus-systeem wordt unit voor unit
    // afgetast, een cloud moet antwoorden. Zonder dit staat er al die tijd een
    // leeg vak en lijkt de pagina vast te zitten.
    const content = $('pair-content');
    content.replaceChildren(node('p', 'empty busy', 'Zoeken naar apparaten…'));
    try {
      const candidates = await pairEmit('list_devices', null);
      renderCandidates(candidates || []);
    } catch (error) {
      // De melding hoort in het vak te staan waar de gebruiker naar kijkt, niet
      // alleen in een toast die na drie seconden weg is.
      content.replaceChildren(node('p', 'empty bad', error.message || String(error)));
      throw error;
    }
  }
}
// De koppelstap voor een toestel dat meldingen ontvangt.
//
// Deze stap tekent Manage zelf in plaats van de app, en dat is geen luiheid maar
// noodzaak: een koppelpagina van een app staat in een sandbox zonder eigen
// herkomst, en daarin mag een browser geen service worker registreren en dus geen
// abonnement aanvragen. Alleen de bovenste pagina kan dat.
//
// Wat er over de grens gaat is precies wat pairing altijd doet: de browser geeft
// de identiteit af van het ding dat gekoppeld wordt. Dat het hier om zichzelf gaat
// verandert daar niets aan -- de driver bewaart het abonnement in de data van het
// device, net zoals een lamp zijn serienummer bewaart.
async function showPushSubscriptionView() {
  const content = $('pair-content');
  content.replaceChildren();

  const blocker = pushBlocker();
  if (blocker) return void content.append(node('p', 'empty bad', blocker));
  if (!state.pair.handlers.includes('publicKey')) {
    return void content.append(node('p', 'empty bad', 'Deze app levert geen sleutel om je aan te melden.'));
  }

  const form = node('form', 'matter-pair-form');
  const label = node('label', 'settings-field');
  const input = node('input');
  input.type = 'text';
  input.maxLength = 80;
  input.required = true;
  input.value = suggestedPhoneName();
  input.placeholder = 'Bijvoorbeeld iPhone van Derek';
  label.append(node('span', '', 'Naam van dit apparaat'), input);
  const hint = node('small', 'settings-hint', 'Onder deze naam kies je hem in een Flow. Dit is de browser waarin je nu kijkt.');
  const status = node('p', 'hint', '');
  const submit = node('button', 'primary');
  submit.type = 'submit';
  submit.textContent = 'Koppel dit apparaat';
  const actions = node('div', 'actions');
  actions.append(submit);
  form.append(label, hint, status, actions);
  content.append(form);
  input.select();

  form.addEventListener('submit', async event => {
    event.preventDefault();
    const name = input.value.trim();
    if (!name) return;
    submit.disabled = true;
    status.className = 'hint busy';
    status.textContent = 'Aanmelden bij je browser…';
    try {
      // Toestemming eerst, en meteen na de klik: een browser stelt die vraag
      // alleen na een echte handeling, en op een iPhone wordt daar streng over
      // gedaan.
      if (await Notification.requestPermission() !== 'granted') {
        throw new Error('Je hebt meldingen niet toegestaan.');
      }
      const answer = await pairEmit('publicKey', null);
      const publicKey = answer?.publicKey || answer;
      if (typeof publicKey !== 'string' || !publicKey) throw new Error('De app gaf geen geldige sleutel.');
      await navigator.serviceWorker.register('/sw.js', { scope: '/' });
      const registration = await navigator.serviceWorker.ready;
      const subscription = await registration.pushManager.subscribe({
        // userVisibleOnly is verplicht: de browser staat pushen alleen toe onder
        // de belofte dat er ook echt iets te zien is.
        userVisibleOnly: true,
        applicationServerKey: base64UrlToBytes(publicKey),
      });
      await addCandidate({
        name,
        data: {
          endpoint: subscription.endpoint,
          p256dh: bytesToBase64Url(subscription.getKey('p256dh')),
          auth: bytesToBase64Url(subscription.getKey('auth')),
        },
      });
    } catch (error) {
      status.className = 'app-error';
      status.textContent = error.message || String(error);
      submit.disabled = false;
    }
  });
}

function renderCandidates(candidates) {
  const content = $('pair-content'); content.replaceChildren();
  const choices = node('div', 'choices');
  for (const candidate of candidates) {
    const button = node('button', 'choice', candidate.name || candidate.data?.id || 'Device');
    button.addEventListener('click', () => addCandidate(candidate)); choices.append(button);
  }
  if (!candidates.length) choices.append(node('p', 'empty', 'Geen devices gevonden. Controleer eerst de app-settings.'));
  content.append(choices);
}
async function pairEmit(event, data) {
  return api(`/api/stulp/pair/${encode(state.pair.id)}/emit/${encode(event)}`, { method: 'POST', body: JSON.stringify(data) });
}
async function addCandidate(candidate) {
  try {
    await api(`/api/stulp/apps/${encode(state.pair.appId)}/drivers/${encode(state.pair.driverId)}/pair/devices`, { method: 'POST', body: JSON.stringify(candidate) });
    $('pair-dialog').close(); await load();
  } catch (error) { toast(error.message, true); }
}
async function closePair() {
  $('pair-frame').src = 'about:blank';
  if (!state.pair) return;
  const id = state.pair.id; state.pair = null;
  await api(`/api/stulp/pair/${encode(id)}`, { method: 'DELETE' }).catch(() => {});
}

window.addEventListener('message', async event => {
  const message = event.data || {};
  if (message.channel !== 'stulp-plugin') return;
  if (event.source !== $('settings-frame').contentWindow && event.source !== $('pair-frame').contentWindow) return;
  let result, error;
  const settingsMutation = event.source === $('settings-frame').contentWindow && (message.action === 'set' || message.action === 'unset');
  if (settingsMutation) $('settings-status').textContent = 'Opslaan…';
  try {
    const context = message.context || {};
    if (event.source === $('settings-frame').contentWindow && context.appId !== state.settingsAppId) throw new Error('Ongeldige app UI-context');
    if (event.source === $('pair-frame').contentWindow && (context.appId !== state.pair?.appId || context.sessionId !== state.pair?.id)) throw new Error('Ongeldige pair UI-context');
    result = await handlePluginAction(message.action, message.args || {}, context);
  }
  catch (caught) { error = caught.message; }
  if (settingsMutation) $('settings-status').textContent = error || 'Opgeslagen';
  event.source.postMessage({ channel: 'stulp-plugin-response', id: message.id, result, error }, '*');
});
async function handlePluginAction(action, args, context) {
  if (action === 'get') return getAppSetting(context.appId, args.name);
  if (action === 'set') return api(`/api/manager/apps/app/${encode(context.appId)}/setting/${encode(args.name)}`, { method: 'PUT', body: JSON.stringify({ value: args.value }) });
  if (action === 'unset') return api(`/api/manager/apps/app/${encode(context.appId)}/setting/${encode(args.name)}`, { method: 'DELETE' });
  if (action === 'api') return api(`/api/stulp/apps/${encode(context.appId)}/api/${args.path.replace(/^\//, '')}`, { method: args.method, body: args.method === 'GET' ? undefined : JSON.stringify(args.body || {}) });
  if (action === 'emit') return pairEmit(args.event, args.data);
  if (action === 'showView') return showPairView(args.view);
  if (action === 'nextView') return true;
  if (action === 'prevView') return true;
  if (action === 'close') return $('pair-dialog').close();
  if (action === 'title') { $('pair-title').textContent = args.title; return true; }
  // Een pagina in de sandbox mag zelf geen venster openen: allow-popups staat
  // er met opzet niet bij. Een OAuth-koppeling moet de gebruiker wél ergens
  // naartoe kunnen sturen, dus dat gaat langs hier -- alleen http en https, want
  // javascript: of file: hoort een app niet te kunnen aanreiken.
  if (action === 'openURL') {
    let target;
    try { target = new URL(args.url); } catch { return false; }
    if (target.protocol !== 'https:' && target.protocol !== 'http:') return false;
    window.open(target.href, '_blank', 'noopener,noreferrer');
    return true;
  }
  if (action === 'alert') { toast(args.message, args.type === 'error'); return true; }
  if (action === 'confirm') return confirm(args.message);
  return true;
}
async function getAppSetting(appId, name) {
  const response = await fetch(`/api/manager/apps/app/${encode(appId)}/setting/${encode(name)}`);
  if (response.status === 404) return null;
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.error_description || `HTTP ${response.status}`);
  return body;
}

// De stream komt als gewone HTTP binnen: Stulp geeft door wat de plugin
// bedient. Geen peerverbinding, geen ICE, geen SDP -- een <video> met een src.
let videoStop = null;

function startVideo(device, media) {
  stopVideo();
  $('video-title').textContent = media.title || device.name;
  $('video-status').textContent = 'Verbinden…';
  $('video-dialog').showModal();
  const url = `/api/stulp/devices/${encode(device.id)}/media/${encode(media.slot)}/stream?kind=video`;
  playStream(url).catch(error => {
    $('video-status').textContent = error.message || String(error);
  });
}

// Een live camera is een fragmented MP4 zonder einde, en dat speelt een
// <video src> niet af: Safari toont het eerste beeld en blijft daarna staan.
// Vandaar Media Source Extensions -- de fragmenten worden zelf ingelezen en aan
// de speler gevoerd. Het volledige mediatype komt uit de Content-Type die de
// plugin meestuurt, want zonder codec erin weet de browser niet of hij het kan.
async function playStream(url) {
  const video = $('camera-video');
  const response = await fetch(url);
  if (!response.ok || !response.body) {
    throw new Error(response.status === 404 ? 'De app levert deze stream niet.' : `De stream gaf ${response.status}.`);
  }
  const type = response.headers.get('content-type') || 'video/mp4';

  if (typeof MediaSource === 'undefined' || !MediaSource.isTypeSupported(type)) {
    // Geen MSE, of deze browser kent de codec niet. Dan is doorgeven aan de
    // speler het enige wat er nog te proberen valt.
    //
    // Meteen zeggen dat dit die laatste poging is, want een <video src> op een
    // stream zonder einde komt vaak niet verder en geeft ook geen fout: dan
    // stond hier "Verbinden…" tot je het venster sloot, en dat is niet te
    // onderscheiden van een camera die niets stuurt.
    response.body.cancel();
    $('video-status').textContent = `Deze browser kan ${type} niet als livestream inlezen; laatste poging als gewone video…`;
    video.src = url;
    video.addEventListener('playing', () => { $('video-status').textContent = ''; }, { once: true });
    video.addEventListener('error', () => {
      $('video-status').textContent = `Deze browser speelt ${type} niet af.`;
    }, { once: true });
    videoStop = () => { video.removeAttribute('src'); video.load(); };
    await video.play().catch(() => {});
    return;
  }

  const source = new MediaSource();
  const reader = response.body.getReader();
  let stopped = false;

  // Wat er binnenkwam en wat de speler ervan aannam. Alleen met die twee is een
  // stilstaand venster te verklaren: geen bytes is een camera die niets stuurt,
  // bytes zonder aangenomen fragmenten is een speler die de codec afwijst, en
  // beide wél zonder beeld is iets aan de tijdlijn. Zonder dit onderscheid bleef
  // er alleen "Verbinden…" staan, en dat zegt over geen van de drie iets.
  let received = 0;
  let appended = 0;
  const stall = setTimeout(() => {
    if (stopped || !video.paused) return;
    if (received === 0) {
      fail('Er komt geen beeld binnen van deze camera.');
    } else if (appended === 0) {
      fail(`Deze browser nam ${type} niet aan.`);
    } else {
      fail('Er komt beeld binnen, maar de speler start niet.');
    }
  }, 15000);
  const settled = () => clearTimeout(stall);

  // De eerste fout blijft staan.
  //
  // Eén fout veroorzaakt de volgende: struikelt de speler, dan mislukt elke
  // appendBuffer daarna met "the HTMLMediaElement.error attribute is not null" --
  // een mededeling over de vorige fout, niet over deze. Wie dat als laatste te
  // zien krijgt weet nog niets. Dus wint wie er als eerste was.
  let failure = '';
  const fail = message => {
    if (failure) return;
    failure = message;
    settled();
    $('video-status').textContent = message;
  };

  videoStop = () => {
    stopped = true;
    settled();
    reader.cancel().catch(() => {});
    video.removeAttribute('src');
    video.load();
  };
  video.src = URL.createObjectURL(source);

  video.addEventListener('playing', () => { settled(); $('video-status').textContent = ''; }, { once: true });
  video.addEventListener('error', () => {
    fail(video.error?.message
      ? `De speler stopte: ${video.error.message}`
      : `Deze browser speelt ${type} niet af.`);
  }, { once: true });

  source.addEventListener('sourceopen', async () => {
    URL.revokeObjectURL(video.src);
    let buffer;
    try {
      buffer = source.addSourceBuffer(type);
    } catch (error) {
      fail(`Deze browser kan ${type} niet inlezen: ${error.message}`);
      return;
    }
    // Live: wat voorbij is hoeft niet bewaard te blijven, anders groeit de
    // buffer een uur lang door tot de browser hem weggooit en het beeld hapert.
    buffer.mode = 'segments';
    const queue = [];
    const pump = () => {
      if (stopped || failure || buffer.updating || !queue.length) return;
      try {
        buffer.appendBuffer(queue[0]);
        queue.shift();
        appended++;
      } catch (error) {
        if (error.name === 'QuotaExceededError') {
          // Vol. Ruimte maken achter de kijkpositie; updateend komt hier terug.
          const buffered = buffer.buffered;
          if (buffered.length && video.currentTime - buffered.start(0) > 4) {
            try { buffer.remove(buffered.start(0), video.currentTime - 2); } catch { /* volgende ronde */ }
            return;
          }
        }
        // De speler noemt zelf de oorzaak zodra hij er een heeft; de melding van
        // appendBuffer zegt dan alleen dát er al iets mis was.
        fail(`De speler nam dit beeld niet aan: ${video.error?.message || error.message}`);
      }
    };
    // Een fout op de buffer zelf komt asynchroon: dit is waar een beschadigd of
    // niet-passend fragment terechtkomt. Zonder deze regel verdween dat geluidloos
    // en bleef er "Verbinden…" staan.
    buffer.addEventListener('error', () => {
      fail(video.error?.message
        ? `De speler kon dit beeld niet lezen: ${video.error.message}`
        : 'De speler kon dit beeld niet lezen.');
    });
    buffer.addEventListener('updateend', () => {
      const buffered = buffer.buffered;
      if (!buffered.length) return pump();
      // Naar het beeld toe dat er is.
      //
      // Een speler begint op nul en wacht daar. Ligt het beeld ergens anders op
      // de tijdlijn -- wat gebeurt als een plugin ruwe RTP-tijdstempels
      // doorgeeft -- dan wacht hij eeuwig zonder een fout te geven. Eén sprong
      // naar het begin van wat er is maakt dat onschadelijk, ongeacht wat de
      // plugin stuurt.
      if (video.currentTime < buffered.start(0) || video.currentTime > buffered.end(buffered.length - 1)) {
        video.currentTime = buffered.start(0);
      }
      // Alles ouder dan een halve minuut weggooien, maar nooit waar we kijken.
      else if (video.currentTime - buffered.start(0) > 30 && !buffer.updating) {
        try { buffer.remove(buffered.start(0), video.currentTime - 10); } catch { /* niet erg */ }
      }
      pump();
    });

    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done || stopped) break;
        received += value.byteLength;
        queue.push(value);
        pump();
        if (video.paused) video.play().catch(() => {});
      }
    } catch { /* de verbinding ging dicht; stopVideo ruimt de rest op */ }
    settled();
    if (!stopped && source.readyState === 'open') source.endOfStream();
  }, { once: true });
}

// Volledig scherm.
//
// De omhullende doos gaat naar volledig scherm en niet het <video>-element zelf:
// zo blijft de statusregel erop staan als de camera wegvalt, in plaats van dat je
// naar zwart kijkt zonder te weten waarom. Op een iPhone bestaat die keuze niet
// -- daar kan alleen de speler zelf, en dan is dat de weg.
function fullscreenElement() {
  return document.fullscreenElement || document.webkitFullscreenElement || null;
}

function toggleVideoFullscreen() {
  const video = $('camera-video');
  if (fullscreenElement()) {
    (document.exitFullscreen || document.webkitExitFullscreen)?.call(document);
    return;
  }
  const frame = video.parentElement;
  if (frame.requestFullscreen) {
    frame.requestFullscreen().catch(error => toast(error.message, true));
  } else if (video.webkitEnterFullscreen) {
    video.webkitEnterFullscreen();
  } else {
    toast('Deze browser heeft geen volledig scherm.', true);
  }
}

function markVideoFullscreen() {
  const on = Boolean(fullscreenElement());
  const button = $('video-fullscreen');
  const label = on ? 'Volledig scherm verlaten' : 'Volledig scherm';
  button.replaceChildren(materialIcon(on ? 'fullscreen_exit' : 'fullscreen'));
  button.setAttribute('aria-label', label);
  button.title = label;
}

function stopVideo() {
  const video = $('camera-video');
  if (fullscreenElement()) (document.exitFullscreen || document.webkitExitFullscreen)?.call(document);
  video.pause();
  if (videoStop) { videoStop(); videoStop = null; return; }
  // removeAttribute en niet src = '': een lege src laat de browser de pagina
  // zelf ophalen als video, en dan blijft de verbinding met Stulp gewoon staan.
  video.removeAttribute('src');
  video.load();
}

let toastTimer;
function toast(message, error = false) { const target = $('toast'); target.textContent = message; target.className = `show${error ? ' error' : ''}`; clearTimeout(toastTimer); toastTimer = setTimeout(() => { target.className = ''; }, 3000); }

function scheduleRealtimeReload() {
  clearTimeout(realtimeReloadTimer);
  realtimeReloadTimer = setTimeout(load, 120);
}

function applyRealtimeDevice(updated) {
  if (!updated?.id || !updated.capabilitiesObj) return scheduleRealtimeReload();
  const index = state.devices.findIndex(device => device.id === updated.id);
  if (index < 0) return scheduleRealtimeReload();
  const previous = state.devices[index];
  state.devices[index] = {
    ...updated, media: previous.media, mediaLoaded: previous.mediaLoaded, mediaLoading: previous.mediaLoading,
  };
  if (state.deviceOrder) return;
  renderDevices();
  refreshOpenDevicePopover(state.devices[index]);
}

function handleRealtimeEvent(event) {
  if (event.manager === 'notifications' && event.type === 'notification.create') {
    const excerpt = event.data?.excerpt;
    if (excerpt) toast(excerpt);
    return;
  }
  // The stream fell behind and was emptied: what was missed cannot be derived,
  // so the only honest answer is to ask for everything again.
  if (event.manager === 'store') {
    scheduleRealtimeReload();
    return;
  }
  if (event.manager === 'devices' && event.type === 'device.update') {
    if (realtimeDeviceUpdatesDuringLoad && event.data?.id && event.data.capabilitiesObj) {
      realtimeDeviceUpdatesDuringLoad.set(event.data.id, event.data);
    }
    applyRealtimeDevice(event.data);
    return;
  }
  if (event.manager === 'devices' || event.manager === 'apps' || event.manager === 'scene' || event.manager === 'flow') {
    scheduleRealtimeReload();
  }
}

async function connectRealtime() {
  let retry = 500;
  while (!realtimeAbort.signal.aborted) {
    try {
      const response = await fetch('/api/stulp/events', { signal: realtimeAbort.signal });
      if (!response.ok || !response.body) throw new Error(`events HTTP ${response.status}`);
      // The stream is open before this snapshot is read. Events produced
      // during the read wait in the response and are applied afterwards, so
      // reconnecting cannot leave an unnoticed gap.
      await load();
      $('connection').textContent = 'Online · live';
      retry = 500;
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffered = '';
      while (true) {
        const chunk = await reader.read();
        if (chunk.done) break;
        buffered += decoder.decode(chunk.value, { stream: true }).replaceAll('\r\n', '\n');
        let boundary;
        while ((boundary = buffered.indexOf('\n\n')) >= 0) {
          const frame = buffered.slice(0, boundary); buffered = buffered.slice(boundary + 2);
          const data = frame.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n');
          if (!data) continue;
          try { handleRealtimeEvent(JSON.parse(data)); } catch (_) { /* A malformed event is isolated from later frames. */ }
        }
      }
      throw new Error('event stream gesloten');
    } catch (error) {
      if (realtimeAbort.signal.aborted) return;
      $('connection').textContent = 'Opnieuw verbinden…';
      await new Promise(resolve => setTimeout(resolve, retry));
      retry = Math.min(retry * 2, 10000);
    }
  }
}

document.querySelectorAll('.tab').forEach(button => button.addEventListener('click', () => {
  document.querySelectorAll('.tab').forEach(item => item.classList.toggle('active', item === button));
  document.querySelectorAll('.page').forEach(page => page.classList.toggle('active', page.id === `${button.dataset.page}-page`));
  // The map costs a request, so it is fetched when the page is first opened
  // rather than on every reload of everything else.
}));
document.querySelectorAll('[data-device-tab]').forEach(button => button.addEventListener('click', () => showDeviceTab(button.dataset.deviceTab)));
$('device-popover').addEventListener('close', () => {
  state.openDeviceID = null;
  state.deviceTab = 'overview';
  $('device-overview').replaceChildren();
  $('device-settings').replaceChildren();
});
document.querySelectorAll('[data-close]').forEach(button => button.addEventListener('click', () => $(button.dataset.close).close()));
$('add-device').addEventListener('click', chooseDriver);
$('add-group').addEventListener('click', () => openGroupEditor());
$('group-form').addEventListener('submit', saveGroup);
$('group-delete').addEventListener('click', deleteGroup);
$('group-dialog').addEventListener('close', () => { state.editingGroup = null; $('group-form').reset(); });
$('scene-form').addEventListener('submit', saveScene);
$('scene-capture').addEventListener('click', captureWholeScene);
$('scene-clear').addEventListener('click', () => { state.editingScene.states = []; renderSceneEditor(); });
$('scene-dialog').addEventListener('close', () => { state.editingScene = null; $('scene-form').reset(); });
$('add-flow').addEventListener('click', () => openFlow());
$('flow-form').addEventListener('submit', saveFlow);
$('flow-add').addEventListener('click', () => openFlowTypePicker());
document.querySelectorAll('[data-flow-kind]').forEach(button => button.addEventListener('click', () => {
  $('flow-type-dialog').close();
  openFlowCardPicker(button.dataset.flowKind);
}));
$('flow-canvas').addEventListener('dblclick', event => {
  if (event.target.closest('.flow-step')) return;
  const bounds = $('flow-space').getBoundingClientRect();
  openFlowTypePicker({ x: event.clientX - bounds.left, y: event.clientY - bounds.top });
});
$('flow-test').addEventListener('click', testEditedFlow);
$('flow-card-search').addEventListener('input', renderFlowCardChoices);
$('download-backup').addEventListener('click', downloadBackup);
$('restore-backup').addEventListener('click', openRestore);
$('restore-file').addEventListener('change', describeRestoreFile);
$('restore-form').addEventListener('submit', restoreBackup);
$('notifications-button').addEventListener('click', openNotifications);
$('pair-dialog').addEventListener('close', closePair);
$('settings-frame').addEventListener('load', () => { if (state.settingsAppId) $('settings-status').textContent = 'Gereed'; });
$('settings-dialog').addEventListener('close', () => { $('settings-frame').src = 'about:blank'; state.settingsAppId = null; $('settings-status').textContent = ''; });
$('video-dialog').addEventListener('close', stopVideo);
$('video-fullscreen').addEventListener('click', toggleVideoFullscreen);
// Dubbelklikken op het beeld is wat iedereen probeert.
$('camera-video').addEventListener('dblclick', toggleVideoFullscreen);
document.addEventListener('fullscreenchange', markVideoFullscreen);
document.addEventListener('webkitfullscreenchange', markVideoFullscreen);
// Een knop die niets kan hoort er niet te staan. document.fullscreenEnabled is
// false op een iPhone; daar doet de speler het zelf.
if (!document.fullscreenEnabled && !$('camera-video').webkitEnterFullscreen) {
  $('video-fullscreen').classList.add('hidden');
}
$('flow-card-dialog').addEventListener('close', () => { state.flowPickerKind = null; state.flowPickerDevice = null; state.flowAddPoint = null; });
$('flow-dialog').addEventListener('close', () => {
  if ($('flow-type-dialog').open) $('flow-type-dialog').close();
  if ($('flow-card-dialog').open) $('flow-card-dialog').close();
  flowResizeObserver.disconnect();
  state.editingFlow = null;
  state.flowMove = null;
  state.flowLink = null;
});
window.addEventListener('beforeunload', () => { stopVideo(); realtimeAbort.abort(); });
window.addEventListener('resize', scheduleFlowConnections);
window.addEventListener('pointermove', event => { moveDeviceOrder(event); moveFlowNode(event); updateFlowConnectionPreview(event); });
window.addEventListener('pointerup', event => { finishDeviceOrder(event); finishFlowNodeMove(event); finishFlowConnection(event); });
window.addEventListener('pointercancel', event => { finishDeviceOrder(event, false); finishFlowNodeMove(event); finishFlowConnection(event); });
connectRealtime();

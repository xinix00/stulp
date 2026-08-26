'use strict';

// De config-pagina van de Matter-plugin.
//
// Alles loopt via Stulp.api naar de plugin: vraag, antwoord, klaar. Wat lang
// duurt -- een scan, de mesh bevragen -- draait daar in een goroutine, en deze
// pagina haalt op wat er inmiddels ligt. Dat is geen omweg om een ontbrekende
// pushverbinding: het houdt de last bij de plugin, die een node één keer
// bevraagt hoe vaak deze pagina ook kijkt.

const state = { network: null, bridge: null, scan: null, mesh: null, diagnostics: new Map(), sharing: new Map() };

// Wat er nu gevolgd wordt: sleutel -> {pad, body, wat er met het antwoord moet
// gebeuren}. Eén klok voor allemaal, want een scan en een mesh kunnen tegelijk
// lopen en twee timers zouden elkaar dan opheffen. De sleutel is niet het pad:
// diagnostiek van twee apparaten loopt over hetzelfde pad met een andere body.
const watches = new Map();
let ticker = null;

const { $, node, say } = Stulp;

// watch volgt een lopend verzoek tot de plugin zegt dat hij klaar is. Eén
// seconde is traag genoeg om niets te belasten -- de plugin bevraagt de nodes
// toch maar één keer -- en snel genoeg dat je de kaart ziet groeien terwijl
// slaperige nodes nog antwoorden.
function watch(key, path, body, apply) {
  watches.set(key, { path, body, apply });
  if (ticker === null) ticker = setInterval(tick, 1000);
}

async function tick() {
  for (const [key, { path, body, apply }] of [...watches]) {
    let result;
    try {
      result = await Stulp.api('POST', path, body || {});
    } catch (error) {
      watches.delete(key);
      say(error.message || String(error));
      continue;
    }
    apply(result);
    if (!result.running) watches.delete(key);
  }
  if (watches.size === 0) stopWatching();
}

function stopWatching() {
  if (ticker === null) return;
  clearInterval(ticker);
  ticker = null;
  watches.clear();
}

function formatCount(value) { return Number(value || 0).toLocaleString('nl-NL'); }

function formatDuration(seconds) {
  const total = Number(seconds) || 0;
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days) return `${days}d ${hours}u`;
  if (hours) return `${hours}u ${minutes}m`;
  if (minutes) return `${minutes}m`;
  return `${total}s`;
}

// Een Thread-verbinding wordt beoordeeld op zijn LQI: 0-255, meer is beter.
function linkQuality(lqi) {
  if (lqi === undefined || lqi === null) return '';
  if (lqi >= 150) return 'sterk';
  if (lqi >= 80) return 'redelijk';
  return 'zwak';
}

function actionButton(label, onClick) {
  const button = node('button', '', label);
  button.addEventListener('click', onClick);
  return button;
}

function networkBranch(title, subtitle) {
  const branch = node('article', 'card net-branch');
  const name = node('div', 'name');
  name.append(node('strong', '', title));
  if (subtitle) name.append(node('small', 'muted', subtitle));
  branch.append(name);
  return branch;
}

function networkLine(label, detail, className = '') {
  const line = node('div', `net-line ${className}`.trim());
  line.append(node('span', 'net-line-label', label));
  line.append(node('span', 'net-line-detail', detail));
  return line;
}

// ---- Stulp als Matter Bridge -----------------------------------------------

const bridgeKind = {
	onoff: 'schakelaar/licht', dimmable_light: 'dimbaar licht', window_covering: 'gordijn/zonwering',
	contact_sensor: 'contactsensor', occupancy_sensor: 'bewegingssensor',
	temperature_sensor: 'temperatuursensor', humidity_sensor: 'luchtvochtigheidssensor',
};

async function loadBridge() {
	try {
		state.bridge = await Stulp.api('POST', 'bridge/devices', {});
	} catch (error) {
		state.bridge = { error: error.message || String(error) };
	}
	renderBridge();
}

function renderBridge() {
	const container = $('bridge');
	container.replaceChildren();
	const bridge = state.bridge;
	if (!bridge) return;
	const heading = node('div', 'bridge-heading');
	heading.append(node('div', '', 'Stulp Matter Bridge'));
	heading.append(node('small', 'muted', 'Alleen apparaten die je hier kiest worden geëxporteerd.'));
	container.append(heading);
	if (bridge.error) return container.append(node('p', 'app-error', bridge.error));
	const devices = bridge.devices || [];
	if (!devices.length) return container.append(node('p', 'empty', 'Geen geschikte niet-Matter-apparaten gevonden.'));
	for (const device of devices) {
		const row = node('label', 'bridge-device');
		const toggle = document.createElement('input');
		toggle.type = 'checkbox';
		toggle.checked = Boolean(device.selected);
		toggle.addEventListener('change', () => setBridgeExport(device.deviceId, toggle.checked, toggle));
		row.append(toggle);
		const text = node('span', 'bridge-device-name');
		text.append(node('strong', '', device.name));
		text.append(node('small', 'muted',
			`${bridgeKind[device.kind] || device.kind}${device.endpoint ? ` · endpoint ${device.endpoint}` : ''}${device.available ? '' : ' · niet bereikbaar'}`));
		row.append(text);
		container.append(row);
	}
}

async function setBridgeExport(deviceID, exported, checkbox) {
	checkbox.disabled = true;
	try {
		const result = await Stulp.api('POST', 'bridge/export', { deviceId: deviceID, exported });
		state.bridge.devices = result.devices || [];
		renderBridge();
		say(exported ? 'Apparaat krijgt een vast Matter-endpoint.' : 'Apparaat is niet langer via de Matter Bridge zichtbaar.');
	} catch (error) {
		checkbox.checked = !exported;
		checkbox.disabled = false;
		say(error.message || String(error));
	}
}

// ---- Het netwerk ------------------------------------------------------------

async function loadNetwork() {
  $('refresh').disabled = true;
  try {
    state.network = await Stulp.api('POST', 'network', {});
    say('');
  } catch (error) {
    state.network = { error: error.message || String(error) };
  } finally {
    $('refresh').disabled = false;
    renderNetwork();
  }
}

function renderNetwork() {
  const container = $('network');
  container.replaceChildren();
  const map = state.network;
  if (!map) return container.append(node('p', 'empty', 'Nog niet geladen.'));
  if (map.error) {
    const branch = networkBranch('Matter', 'niet beschikbaar');
    branch.append(node('p', 'app-error', map.error));
    return container.append(branch);
  }

  const fabric = map.fabric || {};
  const nodes = map.nodes || [];
  container.append(networkBranch('Fabric',
    `${fabric.fabricId || '?'} · controller ${fabric.controllerId || '?'} · ${formatCount(nodes.length)} nodes`));
  for (const name of fabric.unplaceable || []) {
    container.append(node('p', 'app-error', `${name} heeft geen bruikbare node-identiteit en staat niet op de kaart.`));
  }
  if (!nodes.length) container.append(node('p', 'empty', 'Nog geen Matter-apparaten gekoppeld.'));
  for (const matterNode of nodes) container.append(renderNodeBranch(matterNode));
  if (state.scan) container.append(renderScanBranch(state.scan));
}

function renderNodeBranch(matterNode) {
  const devices = matterNode.devices || [];
  const branch = networkBranch(devices[0]?.name || matterNode.nodeId,
    `${matterNode.nodeId} · ${matterNode.address || 'adres onbekend'}`);
  branch.append(networkLine('Verbinding', [
    matterNode.credentialed ? 'certificaat ✔' : 'certificaat ✘',
    matterNode.sessionOpen ? 'sessie ✔' : 'sessie ✘',
    matterNode.subscribed ? 'subscription ✔' : 'subscription ✘',
  ].join(' · '), matterNode.sessionOpen ? '' : 'net-down'));
  for (const device of devices) {
    branch.append(node('p', 'net-sub',
      `endpoint ${device.endpoint} — ${device.name}${device.available ? '' : ` (${device.unavailableMessage || 'niet beschikbaar'})`}`));
  }

  // Diagnostiek kost een round trip naar het apparaat zelf, dus die wordt per
  // node opgehaald en alleen als iemand erom vraagt.
  const deviceID = devices[0]?.id;
	const sharing = deviceID ? state.sharing.get(deviceID) : null;
	const window = (state.network?.sharingWindows || []).find(item => item.nodeId === matterNode.nodeId);
  const entry = deviceID ? state.diagnostics.get(deviceID) : null;
  if (deviceID) {
    const actions = node('div', 'card-actions');
		if (window) {
			const revokeButton = actionButton('Sluit deelvenster', () => changeSharing('revoke', deviceID));
			revokeButton.disabled = Boolean(sharing?.running);
			actions.append(revokeButton);
		} else {
			const shareButton = actionButton('Deel via Matter', () => changeSharing('open', deviceID));
			shareButton.disabled = Boolean(sharing?.running);
			actions.append(shareButton);
		}
    actions.append(actionButton(entry ? 'Ververs diagnostiek' : 'Vraag diagnostiek op',
      () => loadDiagnostics(deviceID)));
    branch.append(actions);
  }
	if (sharing?.running) branch.append(node('p', 'empty', 'Beveiligde Matter-deelverbinding openen…'));
	if (sharing?.warning) branch.append(node('p', 'app-error', sharing.warning));
	if (window) renderSharingWindow(branch, window);
  if (!entry) return branch;
  if (entry.loading) { branch.append(node('p', 'empty', 'Node bevragen…')); return branch; }
  if (entry.error) { branch.append(node('p', 'app-error', entry.error)); return branch; }
  if (!entry.data) { branch.append(node('p', 'empty', 'Geen antwoord van deze node.')); return branch; }
  renderDiagnostics(branch, entry.data);
  return branch;
}

function renderSharingWindow(branch, window) {
	const expires = new Date(window.expiresAt);
	branch.append(networkLine('Matter Multi-Admin', `open tot ${expires.toLocaleTimeString('nl-NL')}`, 'net-service'));
	const code = node('div', 'matter-code');
	code.append(node('strong', '', window.manualCode));
	code.append(actionButton('Kopieer code', () => copyText(window.manualCode)));
	branch.append(code);
	const qr = node('details', 'matter-payload');
	qr.append(node('summary', '', 'QR-payload tonen'));
	qr.append(node('code', '', window.qrCode));
	qr.append(actionButton('Kopieer payload', () => copyText(window.qrCode)));
	branch.append(qr);
	if (window.wholeBridge) {
		branch.append(node('p', 'app-error',
			'Dit is een Matter-bridge. De ontvangende controller krijgt alle apparaten van deze bridge te zien; één child los delen ondersteunt Matter niet.'));
	}
	const scope = (window.devices || []).map(device => device.name).filter(Boolean);
	if (scope.length) branch.append(node('p', 'net-sub', `Omvang: ${scope.join(' · ')}`));
}

async function copyText(value) {
	try {
		await navigator.clipboard.writeText(value);
		say('Gekopieerd.');
	} catch (_) {
		say('Kopiëren lukte niet; selecteer de code handmatig.');
	}
}

function applySharing(deviceID, action, result) {
	state.sharing.set(deviceID, result);
	if (result.window && state.network) {
		state.network.sharingWindows = (state.network.sharingWindows || [])
			.filter(item => item.nodeId !== result.window.nodeId);
		state.network.sharingWindows.push(result.window);
	}
	if (result.revoked && state.network) {
		state.network.sharingWindows = (state.network.sharingWindows || []).filter(window =>
			window.deviceId !== deviceID && !(window.devices || []).some(device => device.id === deviceID));
	}
	renderNetwork();
	if (!result.running) {
		if (result.warning) say(result.warning);
		else say(action === 'open' ? 'Matter-deelvenster is open.' : 'Matter-deelvenster is gesloten.');
	}
}

async function changeSharing(action, deviceID) {
	state.sharing.set(deviceID, { running: true });
	renderNetwork();
	try {
		const result = await Stulp.api('POST', `sharing/${action}`, {
			deviceId: deviceID,
			...(action === 'open' ? { seconds: 900 } : {}),
		});
		applySharing(deviceID, action, result);
		if (result.running) {
			watch(`sharing:${deviceID}`, 'sharing/state', { deviceId: deviceID },
				update => applySharing(deviceID, action, update));
		}
	} catch (error) {
		state.sharing.set(deviceID, { warning: error.message || String(error) });
		renderNetwork();
	}
}

// Diagnostiek loopt in de plugin en deze pagina kijkt hoe het gaat -- zelfde
// vorm als scan en mesh. Dat is hier geen luxe: een slaperige node mag
// seconden over elk antwoord doen, en zolang de plugin op hém wacht kan hij
// niets anders voor deze pagina doen.
async function loadDiagnostics(deviceID) {
  const apply = result => {
    state.diagnostics.set(deviceID, {
      loading: Boolean(result.running),
      data: result.diagnostics,
      error: result.warning,
    });
    renderNetwork();
  };
  state.diagnostics.set(deviceID, { loading: true });
  renderNetwork();
  try {
    const started = await Stulp.api('POST', 'diagnostics', { deviceId: deviceID });
    apply(started);
    if (started.running) {
      watch(`diagnostics:${deviceID}`, 'diagnostics/state', { deviceId: deviceID }, apply);
    }
  } catch (error) {
    state.diagnostics.set(deviceID, { error: error.message || String(error) });
    renderNetwork();
  }
}

function renderDiagnostics(branch, diagnostics) {
  const identity = [diagnostics.vendorName, diagnostics.productName].filter(Boolean).join(' ');
  const build = [diagnostics.hardwareVersion, diagnostics.softwareVersion].filter(Boolean).join(' / ');
  if (identity || build) branch.append(networkLine(identity || 'Apparaat', build));
  if (diagnostics.serialNumber) branch.append(node('p', 'net-sub', `serienummer ${diagnostics.serialNumber}`));

  const health = [];
  if (diagnostics.upTimeSeconds !== undefined) health.push(`${formatDuration(diagnostics.upTimeSeconds)} aan`);
  if (diagnostics.rebootCount !== undefined) health.push(`${formatCount(diagnostics.rebootCount)}× herstart`);
  if (diagnostics.bootReason) health.push(`laatst: ${diagnostics.bootReason}`);
  if (diagnostics.totalOperationalHours !== undefined) health.push(`${formatCount(diagnostics.totalOperationalHours)} bedrijfsuren`);
  if (health.length) branch.append(networkLine('Toestand', health.join(' · ')));
  for (const fault of diagnostics.activeFaults || []) branch.append(node('p', 'app-error', fault));

  if (diagnostics.thread) renderThread(branch, diagnostics.thread);
  if (diagnostics.wifi) renderWiFi(branch, diagnostics.wifi);
  renderInventory(branch, diagnostics.inventory || []);
  for (const missing of diagnostics.missing || []) {
    branch.append(node('p', 'net-sub', `${missing}: deze node biedt dat niet aan.`));
  }
  for (const error of diagnostics.errors || []) branch.append(node('p', 'app-error', error));
}

function renderInventory(branch, endpoints) {
  if (!endpoints.length) return;
  branch.append(node('h4', 'network-section-title', 'Matter-functies'));
  for (const endpoint of endpoints) {
    const types = (endpoint.deviceTypes || []).join(', ');
    branch.append(networkLine(`Endpoint ${endpoint.endpoint}`, types || 'geen device-type'));
    for (const cluster of endpoint.clusters || []) {
      const counts = [
        `${(cluster.attributes || []).length} attributen`,
        `${(cluster.acceptedCommands || []).length} commando’s`,
        `${(cluster.events || []).length} events`,
      ];
      const label = `${cluster.name || 'Onbekende cluster'} (${cluster.id})`;
      const coverage = cluster.coverage === 'supported' ? 'ondersteund'
        : cluster.coverage === 'partial' ? 'deels gekoppeld'
          : cluster.coverage === 'configuration' ? 'configuratie'
            : cluster.coverage === 'infrastructure' ? 'infrastructuur' : 'nog ongemapt';
      const line = networkLine(label, `${coverage} · ${counts.join(' · ')}`);
      line.classList.add(`net-${cluster.coverage || 'unmapped'}`);
      if (cluster.coverage === 'unmapped') line.classList.add('app-error');
      branch.append(line);
      const gaps = [];
      if ((cluster.unmappedAttributes || []).length) gaps.push(`attributen ${(cluster.unmappedAttributes || []).join(', ')}`);
      if ((cluster.unmappedCommands || []).length) gaps.push(`commando’s ${(cluster.unmappedCommands || []).join(', ')}`);
      if (gaps.length) branch.append(node('p', 'net-sub', `Nog niet gekoppeld: ${gaps.join(' · ')}`));
      for (const error of cluster.errors || []) branch.append(node('p', 'app-error', `${cluster.id}: ${error}`));
    }
  }
}

function renderThread(branch, thread) {
  const radio = [];
  if (thread.routingRole) radio.push(thread.routingRole);
  if (thread.channel !== undefined) radio.push(`kanaal ${thread.channel}`);
  if (thread.panId !== undefined) radio.push(`PAN 0x${Number(thread.panId).toString(16).toUpperCase()}`);
  if (thread.networkName) radio.push(thread.networkName);
  branch.append(networkLine('Thread', radio.join(' · ') || 'geen details'));
  if (thread.overrunCount) branch.append(node('p', 'net-sub', `${formatCount(thread.overrunCount)} overrun(s)`));

  const neighbours = thread.neighbours || [];
  if (!neighbours.length) {
    branch.append(node('p', 'net-sub', 'Geen buren gerapporteerd.'));
    return;
  }
  branch.append(networkLine('Buren', `${formatCount(neighbours.length)} radioverbinding(en)`, 'net-service'));
  for (const neighbour of neighbours) {
    const parts = [];
    if (neighbour.lqi !== undefined) parts.push(`LQI ${neighbour.lqi} (${linkQuality(neighbour.lqi)})`);
    if (neighbour.averageRssi !== undefined) parts.push(`${neighbour.averageRssi} dBm`);
    if (neighbour.frameErrorRate) parts.push(`${neighbour.frameErrorRate}% frame-fouten`);
    if (neighbour.isChild) parts.push('kind');
    if (neighbour.rxOnWhenIdle === false) parts.push('slaapt');
    const weak = neighbour.lqi !== undefined && neighbour.lqi < 80;
    branch.append(networkLine(neighbour.extAddress || `rloc ${neighbour.rloc16}`,
      parts.join(' · ') || 'geen meetwaarden', weak ? 'net-down' : ''));
  }
}

function renderWiFi(branch, wifi) {
  const radio = [];
  if (wifi.rssi !== undefined) radio.push(`${wifi.rssi} dBm`);
  if (wifi.channel !== undefined) radio.push(`kanaal ${wifi.channel}`);
  if (wifi.securityType) radio.push(wifi.securityType);
  if (wifi.version) radio.push(wifi.version);
  branch.append(networkLine('Wi-Fi', radio.join(' · ') || 'geen details', (wifi.rssi ?? 0) < -80 ? 'net-down' : ''));
  const counters = [];
  if (wifi.beaconLostCount) counters.push(`${formatCount(wifi.beaconLostCount)} bakens gemist`);
  if (wifi.packetUnicastRx !== undefined) counters.push(`${formatCount(wifi.packetUnicastRx)} ontvangen`);
  if (wifi.packetUnicastTx !== undefined) counters.push(`${formatCount(wifi.packetUnicastTx)} verzonden`);
  if (counters.length) branch.append(node('p', 'net-sub', counters.join(' · ')));
}

// ---- Wat er op het netwerk te horen is --------------------------------------

function renderScanBranch(scan) {
  const when = scan.finishedAt || scan.startedAt;
  const branch = networkBranch('Op het netwerk gehoord',
    [scan.window, when && new Date(when).toLocaleTimeString('nl-NL'),
      scan.running ? 'bezig…' : ''].filter(Boolean).join(' · '));
  if (scan.warning) branch.append(node('p', 'app-error', scan.warning));

  const groups = [
    ['Operationele nodes', scan.operational, item => {
      const timing = [];
      if (item.idleIntervalMs) timing.push(`slaapt ${item.idleIntervalMs}ms`);
      if (item.activeIntervalMs) timing.push(`actief ${item.activeIntervalMs}ms`);
      // Een DNS-SD-antwoord draagt niet altijd adressen; de hostnaam is dan nog
      // steeds een bruikbare identiteit en beter dan niets tonen.
      const where = (item.addresses || [])[0] || item.host;
      return [item.nodeId || item.instance, [where, ...timing].filter(Boolean).join(' · ')];
    }],
    ['Koppelbaar', scan.commissionable, item =>
      [item.deviceName || item.instance,
        [(item.addresses || [])[0] || item.host, `discriminator ${item.discriminator}`,
          item.commissioningMode ? 'koppelmodus open' : 'koppelmodus dicht'].filter(Boolean).join(' · ')]],
    ['Thread border routers', scan.borderRouters, item =>
      [item.networkName || item.instance,
        [item.vendor, item.model, item.extendedPanId && `PAN ${item.extendedPanId}`,
          item.threadVersion && `Thread ${item.threadVersion}`].filter(Boolean).join(' · ')]],
  ];
  let total = 0;
  for (const [title, items, describe] of groups) {
    if (!items?.length) continue;
    total += items.length;
    branch.append(networkLine(title, `${formatCount(items.length)}×`, 'net-service'));
    for (const item of items) {
      const [label, detail] = describe(item);
      branch.append(networkLine(label, detail));
    }
  }
  if (!total && !scan.running) branch.append(node('p', 'empty', 'Niets gehoord.'));
  return branch;
}

function applyScan(result) {
  state.scan = result;
  renderNetwork();
  $('scan').disabled = Boolean(result.running);
  const found = (result.operational || []).length + (result.commissionable || []).length
    + (result.borderRouters || []).length;
  say(result.running ? 'Luisteren op het netwerk…' : result.warning || `${found} gevonden.`);
}

async function scanNetwork() {
  $('scan').disabled = true;
  say('Luisteren op het netwerk…', 'busy');
  try {
    applyScan(await Stulp.api('POST', 'scan', { window: 4 }));
    watch('scan', 'scan/state', {}, applyScan);
  } catch (error) {
    say(error.message || String(error));
    $('scan').disabled = false;
  }
}

// ---- De mesh ----------------------------------------------------------------

function applyMesh(result) {
  state.mesh = result;
  renderMesh();
  $('mesh').disabled = Boolean(result.running);
  if (!result.running) { say(result.warning || meshSummary(result)); return; }
  const nodes = result.nodes || [];
  const answered = nodes.filter(item => !item.pending).length;
  say(`${answered} van ${nodes.length} nodes bevraagd…`, 'busy');
}

async function drawMesh() {
  $('mesh').disabled = true;
  say('Elke node wordt om zijn burenlijst gevraagd, één voor één. Dit duurt.');
  try {
    applyMesh(await Stulp.api('POST', 'mesh', { window: 4 }));
    watch('mesh', 'mesh/state', {}, applyMesh);
  } catch (error) {
    say(error.message || String(error));
    $('mesh').disabled = false;
  }
}

// resume laat zien wat er in de plugin ligt. Een scan of mesh loopt daar door
// als deze pagina dichtgaat, en dan is wat er is tonen eerlijker dan doen alsof
// er niets gebeurd is.
async function resume(path, apply) {
  let result;
  try {
    result = await Stulp.api('POST', path, {});
  } catch { return; }
  if (!result || (!result.startedAt && !result.running)) return;
  apply(result);
  if (result.running) watch(path, path, {}, apply);
}

$('refresh').addEventListener('click', loadNetwork);
$('scan').addEventListener('click', scanNetwork);
$('mesh').addEventListener('click', drawMesh);
window.addEventListener('unload', stopWatching);

Stulp.ready();
Promise.all([loadNetwork(), loadBridge()])
  .then(() => resume('scan/state', applyScan))
  .then(() => resume('mesh/state', applyMesh));

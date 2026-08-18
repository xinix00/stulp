// Het tekenwerk van de mesh.
//
// vis-network doet de opmaak, het schalen, het zoomen en het slepen -- veel werk
// om met de hand te doen en slechter te doen. Wat een lijn betekent wordt hier
// niet bepaald: welke twee meldingen dezelfde verbinding zijn, of beide kanten
// hem bevestigden en hoe goed hij is, is in Go beslist en komt zo binnen. Dit
// zet dat om in vormen en kleuren.

// ---- Mesh-graaf -------------------------------------------------------------
//
// The graph is drawn by vis-network (vendored under /assets/vendor/). It does
// the layout, the rescaling, the zooming and the dragging, which is a great
// deal of work to do by hand and to do worse.
//
// Nothing here decides what the data means: which two reports are the same
// link, whether both ends confirmed it and how good it is are settled in Go and
// arrive with the event. This only maps that onto shapes and colours.

const MESH_COLOURS = {
  live: '#60a5fa', open: '#818cf8', idle: '#64748b', pending: '#303747',
  error: '#fb7185', controller: '#a78bfa', router: '#38bdf8',
  strong: '#4ade80', fair: '#fbbf24', weak: '#fb7185', border: '#818cf8', unknown: '#64748b',
};

let meshNetwork = null;
let meshData = null;

function meshNodeColour(item) {
  if (item.error) return MESH_COLOURS.error;
  if (item.pending) return MESH_COLOURS.pending;
  if (item.subscribed) return MESH_COLOURS.live;
  return item.sessionOpen ? MESH_COLOURS.open : MESH_COLOURS.idle;
}

function meshNodeTitle(item) {
  return [nodeLabel(item), item.nodeId, item.address,
    item.networkName && `Thread-netwerk ${item.networkName}`,
    item.neighbours ? `${item.neighbours} buren` : '',
    item.error].filter(Boolean).join('\n');
}

function nodeLabel(item) { return item.name || item.nodeId; }

function meshNodeEntry(item) {
  const role = item.pending ? 'wordt bevraagd…'
    : item.routingRole || (item.radio === 'wifi' ? 'Wi-Fi' : item.error ? 'geen antwoord' : '');
  return {
    id: item.nodeId,
    label: role ? `${nodeLabel(item)}\n${role}` : nodeLabel(item),
    title: meshNodeTitle(item),
    shape: 'dot',
    // Endpoint count drives the size, so a bridge with many endpoints reads as
    // the bigger thing it is.
    value: Math.max(1, item.endpoints || 1),
    color: { background: meshNodeColour(item), border: '#101012' },
    font: { color: '#f5f5f6', size: 13, multi: false, strokeWidth: 5, strokeColor: '#101012' },
  };
}

function meshRouterEntry(router) {
  return {
    id: router.id,
    label: `${router.networkName || router.name}\n${[router.vendor, router.model].filter(Boolean).join(' ')}`,
    title: [router.name, router.networkName && `Thread-netwerk ${router.networkName}`,
      router.extendedPanId && `PAN ${router.extendedPanId}`,
      (router.addresses || [])[0]].filter(Boolean).join('\n'),
    shape: 'box',
    color: { background: MESH_COLOURS.router, border: '#101012' },
    font: { color: '#ffffff', size: 13, strokeWidth: 0 },
    margin: 9,
  };
}

function meshEdgeEntry(link, names) {
  const border = link.kind === 'border';
  return {
    id: link.id,
    from: link.from,
    to: link.to,
    width: link.weight,
    // A one-sided report is dashed: only a link both ends confirm proves the
    // address behind it.
    dashes: border ? [2, 6] : !link.mutual,
    color: { color: MESH_COLOURS[link.grade] || MESH_COLOURS.unknown, opacity: border ? 0.7 : 1 },
    title: border
      ? `${names(link.from)} hangt aan Thread-netwerk ${names(link.to)}`
      : `${names(link.from)} ↔ ${names(link.to)} · LQI ${link.lqi ?? '?'}` +
        `${link.rssi !== undefined ? ` · ${link.rssi} dBm` : ''}` +
        `${link.mutual ? '' : ' · eenzijdig gemeld'}`,
  };
}

const MESH_OPTIONS = {
  autoResize: true,
  physics: {
    solver: 'forceAtlas2Based',
    forceAtlas2Based: { gravitationalConstant: -70, springLength: 130, springConstant: 0.06, avoidOverlap: 0.6 },
    stabilization: { iterations: 220, updateInterval: 40 },
  },
  interaction: { hover: true, dragNodes: true, dragView: true, zoomView: true, tooltipDelay: 120 },
  nodes: { borderWidth: 2, scaling: { min: 14, max: 30, label: false } },
  edges: { smooth: { type: 'continuous', roundness: 0.2 } },
};

// The controller is not a Matter node, so it is added as its own vertex rather
// than smuggled into the node list.
function meshControllerEntry() {
  return {
    id: '__stulp', label: 'Stulp', shape: 'dot', value: 6,
    color: { background: MESH_COLOURS.controller, border: '#101012' },
    font: { color: '#f5f5f6', size: 14, strokeWidth: 5, strokeColor: '#101012' },
  };
}

function renderMesh() {
  const view = $('mesh-view');
  const mesh = state.mesh;
  if (!mesh) { view.classList.add('hidden'); return; }
  view.classList.remove('hidden');

  const warnings = $('mesh-warnings');
  warnings.replaceChildren();
  for (const warning of mesh.warnings || []) warnings.append(node('p', 'app-error', warning));
  const nodes = mesh.nodes || [];
  const canvas = $('mesh-canvas');
  if (!nodes.length) {
    destroyMesh();
    warnings.append(node('p', 'empty', 'Nog geen Matter-apparaten om te tekenen.'));
    $('mesh-legend').replaceChildren();
    return;
  }

  const names = id => {
    const found = nodes.find(item => item.nodeId === id);
    if (found) return nodeLabel(found);
    const router = (mesh.routers || []).find(item => item.id === id);
    return router ? (router.networkName || router.name) : id;
  };
  const vertices = [meshControllerEntry(),
    ...(mesh.routers || []).map(meshRouterEntry), ...nodes.map(meshNodeEntry)];
  const edges = [
    // The control path: which nodes Stulp is actually talking to.
    ...nodes.map(item => ({
      id: `control|${item.nodeId}`, from: '__stulp', to: item.nodeId,
      width: item.sessionOpen ? 1.4 : 1, dashes: !item.sessionOpen,
      color: { color: item.sessionOpen ? MESH_COLOURS.live : MESH_COLOURS.idle, opacity: 0.35 },
      title: item.sessionOpen ? 'sessie open' : 'geen sessie',
    })),
    ...(mesh.links || []).filter(link => link.to).map(link => meshEdgeEntry(link, names)),
  ];

  if (typeof vis === 'undefined') {
    // Without the vendored library the failure is otherwise a cryptic
    // "cannot read properties of undefined".
    warnings.append(node('p', 'app-error',
      'De graafbibliotheek is niet geladen. Controleer settings/vendor/vis-network.min.js.'));
    return;
  }
  if (!meshNetwork) {
    meshData = { nodes: new vis.DataSet(vertices), edges: new vis.DataSet(edges) };
    meshNetwork = new vis.Network(canvas, meshData, MESH_OPTIONS);
    // Fit once the physics has settled, so the first sight of the graph is the
    // whole graph rather than a corner of it.
    meshNetwork.once('stabilizationIterationsDone', () => meshNetwork.fit({ animation: true }));
  } else {
    // Updating the DataSets rather than rebuilding keeps the positions the user
    // has dragged things to, and lets new nodes settle in among them.
    meshData.nodes.update(vertices);
    meshData.edges.update(edges);
    // Weghalen wat er niet meer is, en dat geldt ook voor lijnen: tijdens het
    // bevragen staat elke gemelde buur er nog, en aan het eind houdt Go alleen
    // over wat het bij een node in dit fabric kon plaatsen. Zonder deze regel
    // blijven de afgevallen lijnen staan als iets dat gemeten lijkt.
    const wantedNodes = new Set(vertices.map(item => item.id));
    const wantedEdges = new Set(edges.map(item => item.id));
    meshData.nodes.remove(meshData.nodes.getIds().filter(id => !wantedNodes.has(id)));
    meshData.edges.remove(meshData.edges.getIds().filter(id => !wantedEdges.has(id)));
  }
  $('mesh-legend').replaceChildren(...meshLegendItems(mesh));
}

function destroyMesh() {
  if (!meshNetwork) return;
  meshNetwork.destroy();
  meshNetwork = null;
  meshData = null;
}

function meshLegendItems(mesh) {
  const items = [];
  const swatch = (colour, label, dash) => {
    const item = node('span', 'mesh-legend-item');
    const mark = node('i', dash ? 'mesh-dash' : 'mesh-dot');
    mark.style.background = colour;
    item.append(mark, document.createTextNode(label));
    return item;
  };
  items.push(swatch(MESH_COLOURS.live, 'subscription actief'));
  items.push(swatch(MESH_COLOURS.open, 'sessie open'));
  items.push(swatch(MESH_COLOURS.idle, 'geen sessie'));
  items.push(swatch(MESH_COLOURS.pending, 'nog bezig'));
  items.push(swatch(MESH_COLOURS.strong, 'LQI ≥ 150', true));
  items.push(swatch(MESH_COLOURS.fair, 'LQI 80–149', true));
  items.push(swatch(MESH_COLOURS.weak, 'LQI < 80', true));
  items.push(swatch(MESH_COLOURS.router, 'border router', true));
  items.push(node('span', 'mesh-legend-item', 'streepjes = eenzijdig gemeld · sleep gerust'));
  if (mesh.unidentified) {
    items.push(node('span', 'mesh-legend-item',
      `${mesh.unidentified} buren horen bij een ander fabric en staan niet op de kaart`));
  }
  return items;
}

// meshSummary beschrijft een afgeronde ronde. Elk veld is optioneel: een ronde
// kan op elk moment eindigen, ook voordat er iets gezegd is.
function meshSummary(mesh) {
  const nodes = mesh?.nodes || [];
  const links = mesh?.links || [];
  if (!nodes.length) return 'Geen nodes ontvangen.';
  const failed = nodes.filter(item => item.error).length;
  return `${nodes.length} nodes · ${links.length} verbindingen` +
    `${failed ? ` · ${failed} gaven geen antwoord` : ''}`;
}

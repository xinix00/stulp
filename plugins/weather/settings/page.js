'use strict';

// De configuratiepagina: welke locaties gevolgd worden en of ze antwoorden.
// Instellen is er niet bij -- Open-Meteo vraagt niets.

const { $, row, say } = Stulp;

const NAMES = {
  clear: 'onbewolkt', partly: 'half bewolkt', cloudy: 'zwaar bewolkt', fog: 'mist',
  drizzle: 'motregen', rain: 'regen', freezing: 'ijzel', snow: 'sneeuw',
  showers: 'buien', thunderstorm: 'onweer',
};

function show(locations) {
  const found = $('found');
  if (!locations.length) {
    found.replaceChildren(row('Nog geen locatie toegevoegd', '—', 'muted'));
    return;
  }
  found.replaceChildren(...locations.map(place => row(
    place.name,
    // temperature komt als meting binnen met de tekst erbij, in de eenheid die
    // dit huis leest. Deze pagina hoeft dus niets van eenheden te weten.
    [place.answered ? (NAMES[place.state] || place.state || 'gemeten') : 'nog geen meting',
      place.temperature?.text,
      `${place.latitude.toFixed(2)}, ${place.longitude.toFixed(2)}`].filter(Boolean).join(' — '),
    place.answered ? '' : 'muted')));
}

async function refresh() {
  $('check').disabled = true;
  say('Kijken…', 'busy');
  try {
    const result = await Stulp.api('POST', 'status', {});
    show(result.locations || []);
    say(result.locations.length
      ? `${result.locations.length} locatie(s) worden gevolgd.`
      : 'Nog geen locatie. Voeg er een toe via Device toevoegen.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('check').disabled = false;
  }
}

$('check').addEventListener('click', refresh);

Stulp.ready();
refresh();

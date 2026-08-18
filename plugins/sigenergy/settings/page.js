'use strict';

// De configuratiepagina: waar staat het systeem, en op welke units.
//
// Proberen vóór opslaan, want een verkeerd adres merk je anders pas als er geen
// enkel apparaat te koppelen blijkt. Het systeem vertelt zelf wat er achter dat
// adres zit.

const { $, row, say } = Stulp;

function fields() {
  return {
    host: $('host').value.trim(),
    port: Number($('port').value) || 502,
    interval: Number($('interval').value) || 10,
    timeout: Number($('timeout').value) || 5,
    units: $('units').value.trim(),
  };
}

// Per unit wat erop staat. Eén unit kan meer dan één ding zijn: in een SigenStor
// zitten de omvormer en de batterij op hetzelfde unit-id.
function showFound(result) {
  $('found').replaceChildren(
    ...(result.found || []).map(answer => row('Unit ' + answer.unit, (answer.offers || []).join(', '))));
}

$('test').addEventListener('click', async () => {
  const values = fields();
  if (!values.host) { say('Vul eerst een adres in.', 'bad'); return; }
  $('test').disabled = true;
  // Elke unit wordt apart gevraagd, dus dit duurt even bij een lang bereik.
  say('Units aftasten…', 'busy');
  try {
    const result = await Stulp.api('POST', 'test', values);
    const count = (result.found || []).length;
    say(`${count} unit${count === 1 ? '' : 's'} gevonden op ${values.host}, van de ${result.units} afgetast.`, 'ok');
    showFound(result);
  } catch (error) {
    say(error.message || String(error), 'bad');
    $('found').replaceChildren();
  } finally {
    $('test').disabled = false;
  }
});

$('form').addEventListener('submit', async event => {
  event.preventDefault();
  const values = fields();
  $('save').disabled = true;
  say('Opslaan…', 'busy');
  try {
    // Het adres gaat als laatste: elke set laat de app opnieuw verbinden, en dat
    // heeft pas zin als de rest er al staat.
    await Stulp.set('port', values.port);
    await Stulp.set('interval', values.interval);
    await Stulp.set('timeout', values.timeout);
    await Stulp.set('units', values.units);
    await Stulp.set('host', values.host);
    say('Opgeslagen. De app verbindt opnieuw.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('save').disabled = false;
  }
});

Stulp.ready();

// Wat er al staat invullen.
Stulp.api('POST', 'status', {}).then(status => {
  if (status.host) $('host').value = status.host;
  if (status.port) $('port').value = status.port;
  if (status.interval) $('interval').value = status.interval;
  if (status.timeout) $('timeout').value = status.timeout;
  if (status.units) $('units').value = status.units;
  if (status.error) say(status.error, 'bad');
  else if (status.connected) say(`Verbonden. ${status.devices} apparaten worden uitgelezen.`, 'ok');
}).catch(() => {});

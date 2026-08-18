'use strict';

// De configuratiepagina: waar staat de console, en met welke sleutel.
//
// Proberen vóór opslaan, want een verkeerde sleutel merk je anders pas als er
// een camera ontbreekt. De console vertelt zelf wat er te vinden is.

const { $, row, say } = Stulp;

function fields() {
  return {
    host: $('host').value.trim(),
    port: Number($('port').value) || 443,
    apiKey: $('apiKey').value.trim(),
  };
}

function showFound(result) {
  const found = [
    ['Camera’s en deurbellen', result.cameras],
    ['Schijnwerpers', result.lights],
    ['Sensoren', result.sensors],
    ['Gongs', result.chimes],
    ['Relais', result.relays],
  ].filter(([, count]) => count);
  $('found').replaceChildren(...found.map(([label, count]) => row(label, count)));
}

$('test').addEventListener('click', async () => {
  const values = fields();
  if (!values.apiKey) { say('Vul eerst een API-key in.', 'bad'); return; }
  $('test').disabled = true;
  say('Verbinden met de console…', 'busy');
  try {
    const result = await Stulp.api('POST', 'test', values);
    const where = result.console ? `${result.console} (${result.version || 'versie onbekend'})` : 'de console';
    say(`Verbonden met ${where}.`, 'ok');
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
    // De sleutel gaat als laatste: elke set laat de app opnieuw verbinden, en
    // dat heeft pas zin als het adres er al staat.
    await Stulp.set('host', values.host);
    await Stulp.set('port', values.port);
    // Alleen schrijven als er iets ingevuld is: het staat er als wachtwoordveld
    // leeg bij, en een lege set zou de opgeslagen sleutel wissen.
    if (values.apiKey) await Stulp.set('apiKey', values.apiKey);
    say('Opgeslagen. De app verbindt opnieuw.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('save').disabled = false;
  }
});

Stulp.ready();

// Wat er al staat invullen. De sleutel niet: die is opgeslagen en hoort niet
// terug te komen op een scherm.
Stulp.api('POST', 'status', {}).then(status => {
  if (status.host) $('host').value = status.host;
  if (status.port) $('port').value = status.port;
  if (status.hasKey) $('apiKey').placeholder = '········ (opgeslagen)';
  if (status.error) say(status.error, 'bad');
  else if (status.connected) say(`Verbonden. ${status.devices} apparaten worden gevolgd.`, 'ok');
}).catch(() => {});

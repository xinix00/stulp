'use strict';

// De configuratiepagina: met welk Somfy-account, en hoe vaak er gekeken wordt.
//
// Proberen vóór opslaan, want een verkeerd wachtwoord merk je anders pas als er
// niets beweegt. Wat er te koppelen valt komt van de doos zelf -- inclusief de
// apparaten waar deze app nog niets mee doet, want dat is precies de lijst die
// iemand wil zien voordat hij iets mist.

const { $, row, say } = Stulp;

const NAMES = {
  io_roller_shutter: 'Rolluiken',
  io_vertical_exterior_blind: 'Verticale buitenzonwering',
  io_exterior_venetian_blind: 'Buitenjaloezieën',
  io_horizontal_awning: 'Zonneluifels',
  io_velux_roller_shutter: 'Velux-rolluiken',
  io_velux_interior_blind: 'Velux-binnenzonwering',
  io_velux_roof_window: 'Velux-dakramen',
};

function fields() {
  return {
    username: $('username').value.trim(),
    password: $('password').value,
    interval: Number($('interval').value) || 10,
  };
}

function showFound(result) {
  const view = $('found');
  view.replaceChildren();
  const supported = result.supported || {};
  for (const id of Object.keys(NAMES)) {
    if (supported[id]) view.append(row(NAMES[id], supported[id]));
  }
  if (result.scenarios) view.append(row('Scenario’s', result.scenarios));
  const unknown = result.unknown || [];
  if (unknown.length) {
    // Geen fout: dit is wat er nog te porten valt, en het komt van de doos van
    // deze gebruiker en niet uit een lijst hier.
    view.append(row('Nog niet ondersteund', unknown.length, 'muted'));
    for (const name of unknown) view.append(row(name, '—', 'muted'));
  }
}

$('test').addEventListener('click', async () => {
  const values = fields();
  if (!values.username) { say('Vul eerst een gebruikersnaam in.', 'bad'); return; }
  $('test').disabled = true;
  say('Inloggen bij TaHoma…', 'busy');
  try {
    const result = await Stulp.api('POST', 'test', values);
    say(`Ingelogd. TaHoma kent ${result.devices} apparaten.`, 'ok');
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
  $('save').disabled = true;
  say('Opslaan…', 'busy');
  try {
    await Stulp.api('POST', 'save', fields());
    // Het wachtwoord uit het veld halen zodra het bewaard is: het hoeft nergens
    // meer te staan, ook niet in een invoerveld dat open blijft liggen.
    $('password').value = '';
    $('password').placeholder = '········ (opgeslagen)';
    say('Opgeslagen. De app kijkt opnieuw.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('save').disabled = false;
  }
});

$('forget').addEventListener('click', async () => {
  $('forget').disabled = true;
  try {
    await Stulp.api('POST', 'forget', {});
    $('username').value = '';
    $('password').value = '';
    $('password').placeholder = '········';
    $('found').replaceChildren();
    say('De gegevens zijn gewist en er wordt niet meer gekeken.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('forget').disabled = false;
  }
});

Stulp.ready();

// Wat er al staat invullen. Het wachtwoord niet: dat is opgeslagen en hoort niet
// terug te komen op een scherm.
Stulp.api('POST', 'status', {}).then(status => {
  if (status.username) $('username').value = status.username;
  if (status.interval) $('interval').value = status.interval;
  if (status.hasPassword) $('password').placeholder = '········ (opgeslagen)';
  if (status.error) say(status.error, 'bad');
  else if (status.connected) {
    const ago = status.secondsAgo === undefined ? '' : `, laatst gekeken ${status.secondsAgo} s geleden`;
    say(`Verbonden. ${status.devices} apparaten worden gevolgd${ago}.`, 'ok');
  }
}).catch(() => {});

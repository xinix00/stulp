'use strict';

// De configuratiepagina: de registratie bij Spotify, en daarna de koppeling.
//
// Drie stappen omdat er geen cloud-tussenpersoon is: registratie opslaan,
// toestemming geven, en het adres waar je uitkwam terugplakken. De code staat
// in dat adres; er hoeft niets op de redirect te luisteren.

const { $, row, say } = Stulp;

// Waar de gebruiker toestemming geeft. De pagina staat in een sandbox zonder
// allow-popups, dus zelf openen kan niet -- Manage doet het namens ons.
let authorizeURL = '';

function showPlayers(players) {
  $('found').replaceChildren(...(players || []).map(item => row(
    item.name,
    [item.type, item.active ? 'actief' : null, item.restricted ? 'neemt geen opdrachten aan' : null]
      .filter(Boolean).join(' — '),
    item.restricted ? 'muted' : '')));
}

$('form').addEventListener('submit', async event => {
  event.preventDefault();
  $('submit').disabled = true;
  say('Registratie opslaan…', 'busy');
  try {
    await Stulp.set('clientId', $('clientId').value.trim());
    await Stulp.set('redirectUri', $('redirectUri').value.trim());
    const started = await Stulp.api('POST', 'authorize', {});
    authorizeURL = started.url;
    $('step2').hidden = false;
    // Meteen openen: wie op "Opslaan en autoriseren" drukt wil daarheen. De
    // knop hieronder is er voor als de browser het tabblad tegenhoudt.
    Stulp.openURL(authorizeURL);
    $('redirect').focus();
    say('Geef toestemming bij Spotify en plak daarna het adres terug.');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('submit').disabled = false;
  }
});

$('authorize').addEventListener('click', () => {
  if (authorizeURL) Stulp.openURL(authorizeURL);
});

$('finish').addEventListener('submit', async event => {
  event.preventDefault();
  $('link').disabled = true;
  say('Code inwisselen…', 'busy');
  try {
    const result = await Stulp.api('POST', 'exchange', { redirect: $('redirect').value });
    $('step2').hidden = true;
    $('redirect').value = '';
    showPlayers(result.players);
    say(result.players.length
      ? `Gekoppeld. ${result.players.length} speler(s) gevonden — voeg ze toe als apparaat.`
      : 'Gekoppeld, maar Spotify ziet nu geen enkel apparaat. Zet een speaker aan of open de app.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('link').disabled = false;
  }
});

$('check').addEventListener('click', async () => {
  $('check').disabled = true;
  say('Verbinden met Spotify…', 'busy');
  try {
    const result = await Stulp.api('POST', 'check', {});
    showPlayers(result.players);
    say(result.players.length
      ? `Verbonden. ${result.players.length} speler(s) zichtbaar.`
      : 'Verbonden, maar Spotify ziet nu geen enkel apparaat.', 'ok');
  } catch (error) {
    showPlayers([]);
    say(error.message || String(error), 'bad');
  } finally {
    $('check').disabled = false;
  }
});

$('disconnect').addEventListener('click', async () => {
  $('disconnect').disabled = true;
  try {
    await Stulp.api('POST', 'disconnect', {});
    showPlayers([]);
    say('De koppeling is verbroken. De registratie is blijven staan.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('disconnect').disabled = false;
  }
});

Stulp.ready();

// Wat er al staat invullen.
Stulp.api('POST', 'status', {}).then(status => {
  if (status.clientId) $('clientId').value = status.clientId;
  if (status.redirectUri) $('redirectUri').value = status.redirectUri;
  if (status.error) say(status.error, 'bad');
  else if (status.linked) {
    const until = status.expiresAt ? new Date(status.expiresAt).toLocaleTimeString() : null;
    say(`Gekoppeld met Spotify. ${status.devices} speler(s) actief in deze app.`
      + (until ? ` Token geldig tot ${until}; de app vernieuwt het zelf.` : ''), 'ok');
  } else {
    say('Nog niet gekoppeld met Spotify.');
  }
}).catch(() => {});

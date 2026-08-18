'use strict';

// De configuratiepagina: de registratie bij myUplink, en daarna de koppeling.
//
// De koppeling is drie stappen omdat er geen cloud-tussenpersoon is: registratie
// opslaan, autoriseren in de browser, en het adres waar je uitkwam terugplakken.
// De code staat in dat adres; er hoeft niets op de redirect te luisteren.

const { $, row, say } = Stulp;

// Waar de gebruiker inlogt. De pagina staat in een sandbox zonder allow-popups,
// dus zelf openen kan niet -- Manage doet het namens ons.
let authorizeURL = '';

function showPumps(pumps) {
  $('found').replaceChildren(...(pumps || []).map(pump => row(
    [pump.name, pump.system].filter(Boolean).join(' — ') || pump.serial,
    pump.connected ? 'verbonden' : 'niet verbonden')));
}

// Welke van de twee koppelwegen er gekozen is. De browser-weg heeft een
// redirect nodig en een stap 2; de andere heeft aan de knop genoeg.
function method() {
  const chosen = document.querySelector('input[name="method"]:checked');
  return chosen ? chosen.value : 'machine';
}

function showMethod() {
  const browser = method() === 'browser';
  $('redirectField').hidden = !browser;
  $('submit').textContent = browser ? 'Opslaan en autoriseren' : 'Opslaan en koppelen';
  if (!browser) $('step2').hidden = true;
}

for (const radio of document.querySelectorAll('input[name="method"]')) {
  radio.addEventListener('change', showMethod);
}
showMethod();

$('registration').addEventListener('submit', async event => {
  event.preventDefault();
  const browser = method() === 'browser';
  $('submit').disabled = true;
  say('Registratie opslaan…', 'busy');
  try {
    await Stulp.set('clientId', $('clientId').value.trim());
    // Het geheim alleen schrijven als er iets ingevuld is: het staat er als
    // wachtwoordveld leeg bij, en een lege set zou de opgeslagen waarde wissen.
    if ($('clientSecret').value.trim()) {
      await Stulp.set('clientSecret', $('clientSecret').value.trim());
    }

    if (!browser) {
      say('Koppelen met myUplink…', 'busy');
      const result = await Stulp.api('POST', 'connect', {});
      $('step2').hidden = true;
      showPumps(result.pumps);
      say(result.pumps.length
        ? `Gekoppeld. ${result.pumps.length} warmtepomp(en) gevonden — voeg ze toe als apparaat.`
        : 'Gekoppeld, maar deze registratie ziet geen warmtepompen.', 'ok');
      return;
    }

    await Stulp.set('redirectUri', $('redirectUri').value.trim());
    const started = await Stulp.api('POST', 'authorize', {});
    authorizeURL = started.url;
    $('step2').hidden = false;
    // Meteen openen: wie op autoriseren drukt wil daarheen. De knop hieronder
    // is er voor als de browser het tabblad tegenhoudt.
    Stulp.openURL(authorizeURL);
    $('redirect').focus();
    say('Log in bij myUplink en plak daarna het adres terug.');
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
    showPumps(result.pumps);
    say(result.pumps.length
      ? `Gekoppeld. ${result.pumps.length} warmtepomp(en) gevonden — voeg ze toe als apparaat.`
      : 'Gekoppeld, maar dit account heeft geen warmtepompen.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('link').disabled = false;
  }
});

$('check').addEventListener('click', async () => {
  $('check').disabled = true;
  say('Verbinden met myUplink…', 'busy');
  try {
    const result = await Stulp.api('POST', 'check', {});
    showPumps(result.pumps);
    say(`Verbonden. ${result.pumps.length} warmtepomp(en) gevonden.`, 'ok');
  } catch (error) {
    showPumps([]);
    say(error.message || String(error), 'bad');
  } finally {
    $('check').disabled = false;
  }
});

$('disconnect').addEventListener('click', async () => {
  $('disconnect').disabled = true;
  try {
    await Stulp.api('POST', 'disconnect', {});
    showPumps([]);
    say('De koppeling is verbroken. De registratie is blijven staan.', 'ok');
  } catch (error) {
    say(error.message || String(error), 'bad');
  } finally {
    $('disconnect').disabled = false;
  }
});

Stulp.ready();

// Invullen wat er al staat. Het geheim niet: dat is opgeslagen en hoort niet
// terug te komen op een scherm.
Stulp.api('POST', 'status', {}).then(status => {
  if (status.clientId) $('clientId').value = status.clientId;
  if (status.redirectUri) $('redirectUri').value = status.redirectUri;
  if (status.hasSecret) $('clientSecret').placeholder = '········ (opgeslagen)';

  // De keuze staat op de manier waarop er nu gekoppeld is, of anders op de
  // redirect die er al ligt -- dan was de browser-weg blijkbaar de bedoeling.
  const browser = status.linked ? !status.machine : Boolean(status.redirectUri);
  const radio = document.querySelector(`input[name="method"][value="${browser ? 'browser' : 'machine'}"]`);
  if (radio) radio.checked = true;
  showMethod();

  if (status.error) say(status.error, 'bad');
  else if (status.linked) {
    const until = status.expiresAt ? new Date(status.expiresAt).toLocaleTimeString() : null;
    const how = status.machine ? 'op naam van de registratie' : 'op naam van je account';
    say(`Gekoppeld met myUplink (${how}). ${status.devices} pomp(en) actief in deze app.`
      + (until ? ` Token geldig tot ${until}; de app vernieuwt het zelf.` : ''), 'ok');
  } else {
    say('Nog niet gekoppeld met myUplink.');
  }
}).catch(() => {});

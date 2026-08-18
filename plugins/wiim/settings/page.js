'use strict';

// De configuratiepagina: wie er gekoppeld is en of ze antwoorden, plus een
// zoekronde om te zien of multicast in dit huis rondkomt. Dat laatste is de
// enige vraag die je aan een grijze tegel niet kunt zien.

const { $, row, say } = Stulp;

function showPlayers(result) {
  const view = $('players');
  view.replaceChildren();
  const players = result.players || [];
  if (!players.length) {
    view.append(row('Nog geen speler gekoppeld', '—', 'muted'));
    return;
  }
  for (const player of players) {
    let detail = player.address;
    if (player.answers) {
      detail += ` — antwoordde ${player.secondsAgo} s geleden`;
    } else if (player.error) {
      detail += ` — ${player.error}`;
    } else {
      detail += ' — nog geen antwoord';
    }
    view.append(row(player.name, detail, player.answers ? '' : 'muted'));
  }
}

function showFound(result) {
  const view = $('found');
  view.replaceChildren();
  for (const player of result.players || []) {
    view.append(row(player.name, [player.model, player.address].filter(Boolean).join(' — ')));
  }
}

$('search').addEventListener('click', async () => {
  $('search').disabled = true;
  say('Zoeken… dit duurt een paar seconden.', 'busy');
  try {
    const result = await Stulp.api('POST', 'search', {});
    showFound(result);
    if (result.found) say(`${result.found} speler(s) gevonden.`, 'ok');
    else say('Niets gevonden. Multicast komt hier waarschijnlijk niet rond.', 'bad');
  } catch (error) {
    say(error.message || String(error), 'bad');
    $('found').replaceChildren();
  } finally {
    $('search').disabled = false;
  }
});

Stulp.ready();

Stulp.api('POST', 'status', {}).then(showPlayers).catch(() => {});

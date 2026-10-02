'use strict';

(function stulpBridge() {
  const context = window.__STULP_CONTEXT__ || {};
  const strings = window.__STULP_LOCALE__ || {};
  const pending = new Map();
  const listeners = new Map();
  let sequence = 0;

  function request(action, args) {
    const id = `${Date.now()}-${++sequence}`;
    return new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
      parent.postMessage({ channel: 'stulp-plugin', id, action, args, context }, '*');
    });
  }

  function callback(promise, done) {
    if (typeof done === 'function') promise.then(value => done(null, value), error => done(error));
    return promise;
  }

  function translate(key, tags = {}) {
    let value = key.split('.').reduce((node, part) => node && node[part], strings);
    if (typeof value !== 'string') value = key;
    for (const [name, replacement] of Object.entries(tags || {})) {
      value = value.replaceAll(`:${name}`, String(replacement)).replaceAll(`{${name}}`, String(replacement));
    }
    return value;
  }

  window.addEventListener('message', event => {
    const message = event.data || {};
    if (message.channel === 'stulp-plugin-response') {
      const operation = pending.get(message.id);
      if (!operation) return;
      pending.delete(message.id);
      if (message.error) operation.reject(new Error(message.error));
      else operation.resolve(message.result);
    }
    if (message.channel === 'stulp-plugin-event') {
      for (const listener of listeners.get(message.event) || []) listener(message.data);
    }
  });

  // element bouwt één stukje pagina. Zes plugins schreven hier hun eigen
  // variant van, en de een noemde het node en de ander bouwde het met de hand.
  function element(tag, className, text) {
    const made = document.createElement(tag);
    if (className) made.className = className;
    if (text !== undefined && text !== null) made.textContent = text;
    return made;
  }

  const Stulp = {
    __: translate,

    // De paginahulp. Alles hieronder is er omdat het anders in elke plugin
    // opnieuw geschreven wordt: get, set, api en emit geven zelf al een promise
    // terug, dus er hoort ook nergens een new Promise omheen.
    $(id) { return document.getElementById(id); },
    node: element,

    // row is één gevonden ding: naam links, bijzonderheden rechts.
    row(name, detail, className) {
      const made = element('div', className ? 'row ' + className : 'row');
      made.append(element('span', '', name));
      if (detail !== undefined && detail !== null && detail !== '') {
        made.append(element('small', '', detail));
      }
      return made;
    },

    // say zet de melding op de vaste plek van de pagina. kind is 'ok', 'bad' of
    // 'busy' -- zie app-frame.css.
    say(message, kind) {
      const status = document.getElementById('status');
      if (!status) return;
      status.textContent = message || '';
      status.className = 'hint' + (kind ? ' ' + kind : '');
    },

    get(name, done) { return callback(request('get', { name }), done); },
    set(name, value, done) { return callback(request('set', { name, value }), done); },
    unset(name, done) { return callback(request('unset', { name }), done); },
    api(method, path, body, done) { return callback(request('api', { method, path, body }), done); },
    emit(event, data, done) { return callback(request('emit', { event, data }), done); },
    on(event, listener) {
      const current = listeners.get(event) || [];
      current.push(listener);
      listeners.set(event, current);
      return Stulp;
    },
    ready() { request('ready', {}).catch(() => {}); },
    // openURL laat Manage een adres in een nieuw tabblad openen. Zelf openen
    // kan niet: de pagina staat in een sandbox zonder allow-popups, dus een
    // <a target="_blank"> en window.open doen daar allebei niets.
    openURL(url, done) { return callback(request('openURL', { url: String(url) }), done); },
    alert(message, type, done) { return callback(request('alert', { message: String(message), type }), done); },
    confirm(message, type, done) { return callback(request('confirm', { message: String(message), type }), done); },
    setTitle(title) { request('title', { title: String(title) }).catch(() => {}); },
    showView(view) { return request('showView', { view }); },
    nextView() { return request('nextView', {}); },
    prevView() { return request('prevView', {}); },
    close() { return request('close', {}); },
    showLoadingOverlay() { document.documentElement.classList.add('stulp-loading'); },
    hideLoadingOverlay() { document.documentElement.classList.remove('stulp-loading'); },
    setNavigationClose() { request('navigationClose', {}).catch(() => {}); },
  };

  window.Stulp = Stulp;
  window.__ = translate;
  window.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-i18n]').forEach(node => {
      const value = translate(node.getAttribute('data-i18n'));
      if (node instanceof HTMLInputElement && node.placeholder) node.placeholder = value;
      else node.textContent = value;
    });
  });
  window.addEventListener('load', () => {
    if (typeof window.onStulpReady === 'function') window.onStulpReady(Stulp);
  });
})();

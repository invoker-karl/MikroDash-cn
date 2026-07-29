/* eslint-env browser */
(function (global) {
  'use strict';

  var STORAGE_KEY = 'mikrodash-language';
  var DEFAULT_LANGUAGE = 'en-US';
  var SUPPORTED = ['en-US', 'zh-CN'];
  var textState = new WeakMap();
  var attrState = new WeakMap();
  var applying = false;
  var observer = null;

  function normaliseLanguage(value) {
    var raw = String(value || '').toLowerCase();
    if (raw === 'zh' || raw.indexOf('zh-') === 0) return 'zh-CN';
    return DEFAULT_LANGUAGE;
  }

  function initialLanguage() {
    try {
      var saved = localStorage.getItem(STORAGE_KEY);
      if (saved && SUPPORTED.indexOf(saved) !== -1) return saved;
    } catch (_) {}
    return normaliseLanguage(navigator.language || navigator.userLanguage);
  }

  var currentLanguage = initialLanguage();

  function locale() {
    var locales = global.MikroDashLocales || {};
    return locales[currentLanguage] || locales[DEFAULT_LANGUAGE] || { messages: {}, patterns: [] };
  }

  function translateSource(source) {
    if (currentLanguage === DEFAULT_LANGUAGE || !source) return source;
    var active = locale();
    if (Object.prototype.hasOwnProperty.call(active.messages || {}, source)) {
      return active.messages[source];
    }
    var normalisedSource = source.replace(/\s+/g, ' ').trim();
    if (normalisedSource !== source && Object.prototype.hasOwnProperty.call(active.messages || {}, normalisedSource)) {
      return active.messages[normalisedSource];
    }
    var patterns = active.patterns || [];
    for (var i = 0; i < patterns.length; i++) {
      var item = patterns[i];
      var match = source.match(item[0]);
      if (match) {
        return source.replace(item[0], function () {
          var args = Array.prototype.slice.call(arguments, 0, -2);
          return typeof item[1] === 'function' ? item[1].apply(null, args) : item[1];
        });
      }
    }
    return source;
  }

  function translateText(value) {
    var match = String(value || '').match(/^(\s*)([\s\S]*?)(\s*)$/);
    if (!match || !match[2]) return value;
    return match[1] + translateSource(match[2]) + match[3];
  }

  function shouldSkip(node) {
    var parent = node && node.parentElement;
    if (!parent) return false;
    return !!parent.closest('script,style,code,pre,textarea,[data-i18n-skip]');
  }

  function translateTextNode(node) {
    if (!node || node.nodeType !== Node.TEXT_NODE || shouldSkip(node)) return;
    var state = textState.get(node);
    var value = node.nodeValue;
    if (!state || value !== state.rendered) {
      state = { source: value, rendered: value };
      textState.set(node, state);
    }
    var next = currentLanguage === DEFAULT_LANGUAGE ? state.source : translateText(state.source);
    state.rendered = next;
    if (node.nodeValue !== next) node.nodeValue = next;
  }

  function getAttrState(element) {
    var state = attrState.get(element);
    if (!state) {
      state = {};
      attrState.set(element, state);
    }
    return state;
  }

  function translateAttribute(element, name) {
    if (!element.hasAttribute(name)) return;
    var allState = getAttrState(element);
    var value = element.getAttribute(name);
    var state = allState[name];
    if (!state || value !== state.rendered) {
      state = { source: value, rendered: value };
      allState[name] = state;
    }
    var next = currentLanguage === DEFAULT_LANGUAGE ? state.source : translateSource(state.source);
    state.rendered = next;
    if (value !== next) element.setAttribute(name, next);
  }

  function translateElement(element) {
    if (!element || element.nodeType !== Node.ELEMENT_NODE) return;
    if (element.matches('script,style,code,pre,textarea,[data-i18n-skip]')) return;
    translateAttribute(element, 'placeholder');
    translateAttribute(element, 'title');
    translateAttribute(element, 'aria-label');
    for (var i = 0; i < element.childNodes.length; i++) {
      var child = element.childNodes[i];
      if (child.nodeType === Node.TEXT_NODE) translateTextNode(child);
      else if (child.nodeType === Node.ELEMENT_NODE) translateElement(child);
    }
  }

  function syncSelectors() {
    var selectors = document.querySelectorAll('[data-language-select]');
    for (var i = 0; i < selectors.length; i++) selectors[i].value = currentLanguage;
  }

  function applyDocument() {
    if (!document.documentElement) return;
    applying = true;
    document.documentElement.lang = currentLanguage;
    translateElement(document.documentElement);
    syncSelectors();
    applying = false;
  }

  function setLanguage(value) {
    var next = normaliseLanguage(value);
    if (SUPPORTED.indexOf(next) === -1) next = DEFAULT_LANGUAGE;
    currentLanguage = next;
    try { localStorage.setItem(STORAGE_KEY, next); } catch (_) {}
    applyDocument();
    document.dispatchEvent(new CustomEvent('mikrodash:languagechange', { detail: { language: next } }));
  }

  function bindSelectors() {
    document.addEventListener('change', function (event) {
      if (event.target && event.target.matches('[data-language-select]')) setLanguage(event.target.value);
    });
  }

  function startObserver() {
    if (!document.body || observer) return;
    observer = new MutationObserver(function (mutations) {
      if (applying) return;
      applying = true;
      for (var i = 0; i < mutations.length; i++) {
        var mutation = mutations[i];
        if (mutation.type === 'characterData') translateTextNode(mutation.target);
        for (var j = 0; j < mutation.addedNodes.length; j++) {
          var node = mutation.addedNodes[j];
          if (node.nodeType === Node.TEXT_NODE) translateTextNode(node);
          else if (node.nodeType === Node.ELEMENT_NODE) translateElement(node);
        }
      }
      applying = false;
    });
    observer.observe(document.documentElement, { childList: true, characterData: true, subtree: true });
  }

  global.MikroDashI18n = {
    getLanguage: function () { return currentLanguage; },
    setLanguage: setLanguage,
    t: translateSource,
    apply: applyDocument,
    supportedLanguages: SUPPORTED.slice(),
  };

  bindSelectors();
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      applyDocument();
      startObserver();
    });
  } else {
    applyDocument();
    startObserver();
  }
})(window);

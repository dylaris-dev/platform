package main

import (
	"encoding/json"
	"net/url"
	"strings"
)

// The way back into Beam's own settings.
//
// While the panel is reachable the window IS the panel: Beam has no chrome, no
// menu and nothing of its own on screen. The only route to the settings page was
// the "cannot reach the panel" error, so the app's configuration was reachable
// exactly when the app was broken - and the panel list this now opens would have
// been unreachable for anyone whose panel works.
//
// So the shell injects one control into the proxied document. Three things keep
// it from being a nuisance:
//
//   - A shadow root, so no panel stylesheet can reach it and it cannot reach the
//     panel's. The panel is a whole application; a floating div sharing its CSS
//     would eventually collide with something.
//   - Draggable along the bottom edge and remembered per install, because the
//     panel has its own controls down there and where they sit is not something
//     this can know.
//   - It opens the settings page as a NAVIGATION. Rendering Beam's settings
//     inside the panel's document would put them on the panel's origin with the
//     panel's scripts running beside them, and the shell token lives there.
//
// It is deliberately not a "Beam status bar". A fixed strip costs a row of the
// panel on every screen, forever, to show something that matters twice a month.

// launcherStyles is kept apart only so the markup below stays readable. It is
// inlined into the shadow root, which is why it needs no scoping of its own.
const launcherStyles = `
:host { all: initial; }
.bar {
  position: fixed;
  z-index: 2147483000;
  display: flex; gap: 6px;
  font: 500 12px/1.4 system-ui, -apple-system, sans-serif;
}
.dot {
  position: relative;
  width: 34px; height: 34px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 10px;
  background: #12141A;
  border: 1px solid #2A2F38;
  color: #8A95A8;
  cursor: pointer;
  box-shadow: 0 2px 10px rgba(0,0,0,.45);
  transition: color .15s ease, border-color .15s ease, background .15s ease;
  user-select: none; -webkit-user-select: none;
  font: inherit;
  padding: 0;
}
.dot.drag { cursor: grab; }
.dot:hover { color: #D8DDE6; border-color: #3A414D; background: #171A21; }
.dot:active { background: #0E1015; }
.dot:focus-visible, .item:focus-visible, .btn:focus-visible { outline: 2px solid #7048C8; outline-offset: 2px; }
.dot.dragging { cursor: grabbing; opacity: .85; }
.dot[aria-expanded=true] { color: #D8DDE6; border-color: #7048C8; }
/* An update is waiting. */
.dot.has-update::after {
  content: '';
  position: absolute;
  top: -2px; right: -2px;
  width: 9px; height: 9px;
  border-radius: 50%;
  background: #7048C8;
  border: 2px solid #12141A;
}
.dot svg { width: 17px; height: 17px; display: block; }
.menu, .card {
  position: absolute;
  bottom: 42px;
  min-width: 220px; max-width: 300px;
  background: #12141A;
  border: 1px solid #2A2F38;
  border-radius: 12px;
  box-shadow: 0 8px 28px rgba(0,0,0,.5);
  color: #D8DDE6;
}
.menu { padding: 6px; }
.menu[hidden], .card[hidden] { display: none; }
.menu-title { padding: 6px 10px 8px; color: #8A95A8; font-size: 11px; text-transform: uppercase; letter-spacing: .06em; }
.item {
  display: flex; align-items: center; justify-content: space-between; gap: 12px;
  width: 100%; padding: 8px 10px; border: 0; border-radius: 8px;
  background: transparent; color: inherit; font: inherit; text-align: left; cursor: pointer;
}
.item:hover { background: #1B1F27; }
.item:active { background: #0E1015; }
.item[aria-current=true] { color: #B9A2F0; cursor: default; }
.item .host { color: #8A95A8; font-size: 11px; display: block; }
.card { width: 300px; padding: 14px 16px; }
.card-title { font-size: 14px; font-weight: 600; margin: 0 0 4px; }
.card-body { color: #A9B2C2; margin: 0 0 12px; }
.actions { display: flex; justify-content: flex-end; gap: 8px; }
.btn {
  border-radius: 8px; padding: 7px 12px; font: inherit; cursor: pointer;
  border: 1px solid #2A2F38; background: transparent; color: #D8DDE6;
  transition: background .15s ease, border-color .15s ease;
}
.btn:hover { background: #1B1F27; border-color: #3A414D; }
.btn.primary { background: #7048C8; border-color: #7048C8; color: #fff; }
.btn.primary:hover { background: #7E58D4; }
.btn:active { transform: translateY(1px); }
@media (prefers-reduced-motion: reduce) { .dot, .btn { transition: none; } .btn:active { transform: none; } }
`

// launcherScript is the injected control.
//
// Plain DOM, no framework, no fetch: it runs inside somebody else's application
// and the only thing it is allowed to depend on is the platform. Everything it
// knows is stamped in by launcherTag; switching panels and updating are plain
// navigations to the app shell, which is where the shell token lives.
const launcherScript = `
(function () {
  if (window.__beamLauncher) return;
  window.__beamLauncher = true;

  // Fractions, not pixels - see below. A new key rather than the old one: an
  // old pixel value parsed as a fraction clamps to 1, which would look like a
  // deliberate choice the user never made.
  var KEY = 'beam.launcher.fx';
  var LATER = 'beam.update.later';
  var host = document.createElement('div');
  host.setAttribute('data-beam-launcher', '');
  var root = host.attachShadow({ mode: 'closed' });

  var style = document.createElement('style');
  style.textContent = __STYLES__;
  root.appendChild(style);

  var state = __STATE__;
  var hasUpdate = __HAS_UPDATE__;

  var bar = document.createElement('div');
  bar.className = 'bar';
  root.appendChild(bar);

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text) n.textContent = text;
    return n;
  }

  // Panel switcher: only worth a button when there is somewhere to switch to.
  var sw = null, menu = null;
  if (state.panels.length > 1) {
    sw = el('button', 'dot');
    sw.type = 'button';
    sw.title = 'Switch panel';
    sw.setAttribute('aria-label', 'Switch panel');
    sw.setAttribute('aria-haspopup', 'menu');
    sw.setAttribute('aria-expanded', 'false');
    sw.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m16 3 4 4-4 4"/><path d="M20 7H4"/><path d="m8 21-4-4 4-4"/><path d="M4 17h16"/></svg>';
    menu = el('div', 'menu');
    menu.setAttribute('role', 'menu');
    menu.hidden = true;
    menu.appendChild(el('div', 'menu-title', 'Panels'));
    state.panels.forEach(function (p) {
      var item = el('button', 'item');
      item.type = 'button';
      item.setAttribute('role', 'menuitem');
      var label = el('span', '', p.name);
      label.appendChild(el('span', 'host', p.host));
      item.appendChild(label);
      if (p.url === state.active) {
        item.setAttribute('aria-current', 'true');
        item.appendChild(el('span', '', 'Active'));
      } else {
        item.addEventListener('click', function () {
          window.location.href = state.switchRoute + '?to=' + encodeURIComponent(p.url);
        });
      }
      menu.appendChild(item);
    });
    sw.addEventListener('click', function () { setMenu(menu.hidden); });
    bar.appendChild(sw);
    bar.appendChild(menu);
  }
  function setMenu(open) {
    if (!menu) return;
    menu.hidden = !open;
    if (open && card) card.hidden = true;
    sw.setAttribute('aria-expanded', open ? 'true' : 'false');
    alignPopups();
    if (open) { var first = menu.querySelector('.item'); if (first) first.focus(); }
  }

  var btn = el('button', 'dot drag');
  btn.type = 'button';
  btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>';
  bar.appendChild(btn);
  function markUpdate() {
    btn.title = hasUpdate ? 'Dylaris Beam settings - an update is available' : 'Dylaris Beam settings';
    btn.setAttribute('aria-label', btn.title);
    btn.classList.toggle('has-update', hasUpdate);
  }
  markUpdate();

  // The update prompt. "Later" is remembered for this version and this session
  // only; the app asks again after its next hourly check.
  var card = el('div', 'card');
  card.setAttribute('role', 'dialog');
  card.setAttribute('aria-label', 'Update available');
  card.hidden = true;
  var cardBody = el('p', 'card-body');
  var actions = el('div', 'actions');
  var later = el('button', 'btn', 'Later');
  later.type = 'button';
  var now = el('button', 'btn primary', 'Update now');
  now.type = 'button';
  actions.appendChild(later);
  actions.appendChild(now);
  card.appendChild(el('p', 'card-title', 'Update available'));
  card.appendChild(cardBody);
  card.appendChild(actions);
  bar.appendChild(card);
  later.addEventListener('click', function () {
    card.hidden = true;
    try { sessionStorage.setItem(LATER, state.update); } catch (e) { /* private mode */ }
  });
  now.addEventListener('click', function () { window.location.href = state.settingsRoute + '?update=1'; });
  function showPrompt() {
    cardBody.textContent = 'Dylaris Beam ' + state.update + ' is ready to install. You have ' + state.current + '.';
    setMenu(false);
    card.hidden = false;
    alignPopups();
  }
  window.__beamUpdatePrompt = function (v) {
    if (typeof v !== 'string' || !v) return;
    state.update = v;
    hasUpdate = true;
    markUpdate();
    showPrompt();
  };

  // Bottom edge only, and remembered as a FRACTION of the travel rather than as
  // a pixel offset.
  //
  // Pixels do not survive a resize. Dragged hard right in a narrow window and
  // then widened, a stored x of 554 is the middle of the screen - the button
  // wanders, and there is no position a user can pick that stays where they put
  // it. A fraction keeps right at the right, middle in the middle, and left at
  // the left, at every window size.
  var MARGIN = 12, SIZE = 34;
  function travel() {
    SIZE = bar.offsetWidth || SIZE;
    return Math.max(0, window.innerWidth - SIZE - MARGIN * 2);
  }
  function stored() {
    try {
      var v = parseFloat(localStorage.getItem(KEY) || '');
      return isNaN(v) ? null : Math.min(1, Math.max(0, v));
    } catch (e) { return null; }
  }
  // Bottom RIGHT until the user moves it. The left corner is where the panel
  // keeps its sidebar.
  var DEFAULT_F = 1;
  var current = stored() === null ? DEFAULT_F : stored();
  // Popups open toward the middle of the window, so they never hang off it.
  function alignPopups() {
    var right = current > 0.5;
    [menu, card].forEach(function (n) {
      if (!n) return;
      n.style.left = right ? 'auto' : '0';
      n.style.right = right ? '0' : 'auto';
    });
  }
  function placeFraction(f) {
    current = Math.min(1, Math.max(0, f));
    bar.style.left = (MARGIN + current * travel()) + 'px';
    bar.style.bottom = MARGIN + 'px';
    alignPopups();
  }
  function placePixels(x) { placeFraction(travel() === 0 ? 0 : (x - MARGIN) / travel()); }
  placeFraction(current);
  window.addEventListener('resize', function () { placeFraction(current); });

  // A drag must not also be a click.
  var dragging = false, moved = false, offset = 0;
  btn.addEventListener('pointerdown', function (e) {
    dragging = true; moved = false;
    offset = e.clientX - bar.getBoundingClientRect().left;
    btn.classList.add('dragging');
    btn.setPointerCapture(e.pointerId);
  });
  btn.addEventListener('pointermove', function (e) {
    if (!dragging) return;
    if (Math.abs(e.clientX - (bar.getBoundingClientRect().left + offset)) > 3) moved = true;
    placePixels(e.clientX - offset);
  });
  btn.addEventListener('pointerup', function (e) {
    if (!dragging) return;
    dragging = false;
    btn.classList.remove('dragging');
    btn.releasePointerCapture(e.pointerId);
    try { localStorage.setItem(KEY, String(current)); } catch (err) { /* private mode */ }
  });
  btn.addEventListener('click', function () { if (!moved) window.location.href = state.settingsRoute; });

  // Close the menu on Escape or a click anywhere else. Events from inside a
  // closed shadow root are retargeted to the host, which is how "inside" is told.
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && menu && !menu.hidden) { setMenu(false); sw.focus(); }
  });
  document.addEventListener('pointerdown', function (e) {
    if (menu && !menu.hidden && e.target !== host) setMenu(false);
  });

  // Re-attach rather than assuming one insertion holds for the life of the
  // document: this button lives inside somebody else's single-page app, and
  // anything that rebuilds a subtree can take it with it.
  //
  // The subtree matters: React removes nodes from INSIDE body, which an observer
  // with subtree:false never sees.
  //
  // Bounded, and that bound is load-bearing rather than tidiness. Watching the
  // subtree means an append is itself an observed mutation, so if anything in
  // the page removes this node on sight, re-attaching would feed it - a loop
  // that pegs the renderer. A cap turns that worst case into a missing button.
  var attempts = 0, MAX_ATTACHES = 20;
  var observer;
  function attach() {
    if (!document.body || host.isConnected) return;
    if (++attempts > MAX_ATTACHES) { if (observer) observer.disconnect(); return; }
    document.body.appendChild(host);
    placeFraction(current);
  }
  attach();
  observer = new MutationObserver(attach);
  observer.observe(document.documentElement, { childList: true, subtree: true });

  var dismissed = null;
  try { dismissed = sessionStorage.getItem(LATER); } catch (e) { /* private mode */ }
  if (hasUpdate && dismissed !== state.update) showPrompt();
})();
`

// launcherPanel is one entry of the switcher menu.
type launcherPanel struct {
	Name string `json:"name"`
	Host string `json:"host"`
	URL  string `json:"url"`
}

// launcherState is everything the launcher script knows, stamped in per page.
type launcherState struct {
	Update  string          // newer version waiting, "" when none
	Current string          // this build's version
	Panels  []launcherPanel // saved panels; the switcher shows from two on
	Active  string          // URL of the panel on screen
}

// launcherState reads the saved panels for the switcher. Called per proxied
// HTML page, which in a single-page panel is a handful of times per session.
func (a *App) launcherState() launcherState {
	s := loadSettings()
	panels := []launcherPanel{}
	for _, p := range s.panelList() {
		host := p.URL
		if u, err := url.Parse(p.URL); err == nil && u.Host != "" {
			host = u.Host
		}
		panels = append(panels, launcherPanel{Name: p.DisplayName(), Host: host, URL: p.URL})
	}
	return launcherState{Update: a.updates.get(), Current: AppVersion, Panels: panels, Active: s.activePanel().URL}
}

// launcherTag returns the injected script element, nonce-stamped so a
// nonce-strict policy admits it.
//
// The state is stamped in rather than fetched by the script: the launcher runs
// inside somebody else's application and must not make calls of its own. The
// hourly update check reaches an already-open page through
// window.__beamUpdatePrompt instead (see update_watch.go).
func launcherTag(nonce string, st launcherState) string {
	if st.Panels == nil {
		st.Panels = []launcherPanel{}
	}
	// json.Marshal escapes <, > and &, so no value can close the script element.
	stateJSON, _ := json.Marshal(map[string]interface{}{
		"update":        st.Update,
		"current":       st.Current,
		"panels":        st.Panels,
		"active":        st.Active,
		"settingsRoute": beamSettingsRoute,
		"switchRoute":   beamSwitchRoute,
	})
	body := strings.ReplaceAll(launcherScript, "__STYLES__", goStringLiteral(launcherStyles))
	body = strings.ReplaceAll(body, "__STATE__", string(stateJSON))
	flag := "false"
	if st.Update != "" {
		flag = "true"
	}
	body = strings.ReplaceAll(body, "__HAS_UPDATE__", flag)
	attr := ""
	if nonce != "" {
		attr = ` nonce="` + nonce + `"`
	}
	return "<script" + attr + ">" + body + "</script>"
}

// goStringLiteral renders a string as a JavaScript literal.
//
// The stylesheet is ours and contains no quotes today, which is exactly why this
// exists: relying on that is how a later edit adding a quoted content rule
// breaks every page the shell serves.
func goStringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '<':
			// Escaped so a rule can never close the script element it lives in.
			b.WriteString("\\u003c")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

"use strict";
// TailLAN UI. No framework, no build step: plain DOM building with
// textContent everywhere, because invite contents come from other people.

// ---------- tiny DOM helper ----------
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else if (k === "checked") el.checked = !!v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}
const main = document.getElementById("main");

// ---------- API ----------
async function call(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  let data = null;
  try { data = await r.json(); } catch { /* empty */ }
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}
const get = (p) => call("GET", p);
const post = (p, b) => call("POST", p, b || {});

function toast(msg, bad) {
  const t = h("div", { class: "toast" + (bad ? " bad" : "") }, msg);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), bad ? 6000 : 2600);
}
async function copy(text, what) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = h("textarea", {}, text);
    document.body.append(ta); ta.select(); document.execCommand("copy"); ta.remove();
  }
  toast((what || "Copied") + " to clipboard");
}
// Run an async action from a button, showing a spinner and errors.
async function busy(btn, fn) {
  const old = btn.innerHTML;
  btn.disabled = true;
  btn.replaceChildren(h("span", { class: "spinner" }), " Working...");
  try { await fn(); } catch (e) { toast(e.message, true); }
  finally { btn.disabled = false; btn.innerHTML = old; }
}

// ---------- state ----------
const S = {
  view: "play",
  state: null,       // from /api/events
  games: [],
  hostPick: null,    // game being set up for hosting (GameInfo or {custom})
  preview: null,     // decoded invite preview
  previewErr: "",
  code: "",
  edit: null,        // game being edited in the Games tab
  detect: null,      // last detector scan
  detectBase: null,  // snapshot for "what's new"
  doctor: null,
  doctorBusy: false,
  logs: [],
  gameQuery: "",
};
const session = () => S.state && S.state.session;
const live = () => { const s = session(); return s && (s.role === "host" || (s.role === "join" && s.phase !== "ended")); };

async function loadGames() { S.games = await get("/api/games"); }

// ---------- events ----------
let lastKey = "";
function connect() {
  const es = new EventSource("/api/events");
  es.addEventListener("state", (e) => {
    document.getElementById("offline").hidden = true;
    S.state = JSON.parse(e.data);
    const s = session();
    // Re-render everything only when the kind of screen changes, so
    // typing in a form isn't interrupted every second.
    const key = [S.view, s ? s.role + ":" + (s.phase || "") : "none"].join("|");
    if (key !== lastKey) { lastKey = key; render(); }
    else refresh();
    renderWho();
  });
  es.onerror = () => {
    document.getElementById("offline").hidden = false;
  };
}

// ---------- header ----------
function renderWho() {
  const el = document.getElementById("who");
  if (el.querySelector("input")) return;
  const name = (S.state && S.state.name) || "";
  el.replaceChildren(
    "Playing as ", h("b", {}, name), " ",
    h("button", { class: "linkbtn small", title: "Change the name friends see", onclick: editName }, "change"));
}
function editName() {
  const el = document.getElementById("who");
  const inp = h("input", { value: S.state.name, maxlength: 40, "aria-label": "Your name" });
  const save = async () => {
    try { await post("/api/settings", { name: inp.value }); S.state.name = inp.value.trim(); }
    catch (e) { toast(e.message, true); }
    el.replaceChildren(); renderWho();
  };
  inp.addEventListener("keydown", (e) => { if (e.key === "Enter") save(); if (e.key === "Escape") { el.replaceChildren(); renderWho(); } });
  inp.addEventListener("blur", save);
  el.replaceChildren("Name ", inp);
  inp.focus(); inp.select();
}

document.getElementById("nav").addEventListener("click", (e) => {
  const v = e.target.dataset && e.target.dataset.view;
  if (v) go(v);
});
function go(view) {
  S.view = view;
  if (location.hash.slice(1) !== view) history.replaceState(null, "", "#" + view);
  for (const b of document.querySelectorAll("#nav button")) b.classList.toggle("active", b.dataset.view === view);
  lastKey = "";
  render();
  main.focus({ preventScroll: true });
  window.scrollTo(0, 0);
}

// ---------- render ----------
function render() {
  const views = { play: viewPlay, games: viewGames, tools: viewTools, help: viewHelp };
  main.replaceChildren(views[S.view]());
}
// refresh updates only the live session panel in place, and only when
// something changed and the user isn't selecting text in it.
let lastSession = "";
function refresh() {
  if (S.view === "play" && session()) {
    const el = document.getElementById("session");
    const js = JSON.stringify(session());
    const sel = document.getSelection();
    const selecting = sel && !sel.isCollapsed && el && el.contains(sel.anchorNode);
    if (el && js !== lastSession && !selecting) {
      lastSession = js;
      el.replaceWith(viewSession());
    }
  }
  if (S.view === "tools") {
    const el = document.getElementById("logs");
    if (el) updateLogs(el);
  }
}

// ===== Play =====
function viewPlay() {
  const s = session();
  if (s) return viewSession();
  if (S.hostPick) return viewHostSetup();
  return h("div", {},
    h("h1", {}, "Play with friends"),
    h("p", { class: "sub" }, "One person hosts and sends an invite code. Everyone else pastes it. That's it: no accounts, no router settings."),
    h("div", { class: "grid2" }, cardJoin(), cardHost()));
}

function cardJoin() {
  const ta = h("textarea", {
    id: "code", placeholder: "Paste the invite code (TL1-...) or the whole message your friend sent",
    rows: 4, value: S.code, spellcheck: "false",
  });
  let timer;
  ta.addEventListener("input", () => {
    S.code = ta.value;
    clearTimeout(timer);
    timer = setTimeout(inspect, 250);
  });
  const pasteBtn = h("button", { class: "btn small", onclick: async () => {
    try { ta.value = S.code = await navigator.clipboard.readText(); inspect(); }
    catch { toast("Press Ctrl+V (Cmd+V on Mac) in the box to paste", true); ta.focus(); }
  } }, "Paste");
  return h("section", { class: "card big" },
    h("h2", {}, "Join a game"),
    h("p", { class: "lead" }, "Got an invite from a friend? Paste it here."),
    h("div", { class: "stack" }, ta, h("div", { class: "row" }, pasteBtn, h("span", { class: "spacer" })), h("div", { id: "preview" }, previewBox())));
}

async function inspect() {
  S.preview = null; S.previewErr = "";
  if (S.code.trim()) {
    try { S.preview = await post("/api/invite/inspect", { code: S.code }); }
    catch (e) { S.previewErr = e.message; }
  }
  const el = document.getElementById("preview");
  if (el) el.replaceChildren(previewBox());
}

function previewBox() {
  if (S.previewErr) return h("div", { class: "note bad" }, S.previewErr);
  const p = S.preview;
  if (!p) return h("div", { class: "muted small" }, "Your friend gets the code by clicking Host a game in TailLAN.");
  const lan = h("input", { type: "checkbox", id: "allowlan" });
  const needsLAN = p.game.ports.some((x) => x.lan);
  return h("div", { class: "stack" },
    h("div", { class: "note ok" },
      h("b", {}, (p.hostName || "Your friend") + " is hosting " + p.game.name),
      h("div", { class: "small" }, "Ports: " + p.portSummary)),
    p.busy && p.busy.length ? h("div", { class: "note warn" },
      "Port " + p.busy.join(", ") + " is already in use on this computer and this game needs exactly that port. Close the program using it (is the game's own server running here?) before joining.") : null,
    needsLAN ? h("label", { class: "check small muted" }, lan,
      h("span", {}, "Also let other devices on my home network use this (e.g. a second PC). Leave off unless you need it.")) : null,
    h("button", { class: "btn primary big", onclick: (e) => busy(e.currentTarget, async () => {
      await post("/api/join", { code: S.code, allowLAN: lan.checked });
      S.code = ""; S.preview = null;
    }) }, "Join game"));
}

function cardHost() {
  const q = h("input", { type: "text", placeholder: "Search games...", value: S.gameQuery, "aria-label": "Search games" });
  const list = h("div", { class: "games-list" });
  const fill = () => {
    const query = q.value.trim().toLowerCase();
    S.gameQuery = q.value;
    const match = (g) => !query || [g.id, g.name, ...(g.aliases || []), ...(g.tags || [])].some((x) => x.toLowerCase().includes(query));
    const tiles = S.games.filter(match).map((g) => h("button", { class: "game-tile", onclick: () => pickHost(g) },
      h("span", { class: "t" }, g.name), h("span", { class: "p" }, g.portSummary)));
    tiles.push(h("button", { class: "game-tile custom", onclick: () => pickHost({ custom: true, name: q.value.trim() }) },
      h("span", { class: "t" }, "Another game (any ports)"),
      h("span", { class: "p muted" }, "Type its ports, or detect them")));
    list.replaceChildren(...tiles);
  };
  q.addEventListener("input", fill);
  fill();
  return h("section", { class: "card big" },
    h("h2", {}, "Host a game"),
    h("p", { class: "lead" }, "Pick the game you're running. Friends join through you."),
    q, list);
}

function pickHost(g) {
  S.hostPick = g;
  render();
}

// ----- host setup -----
function viewHostSetup() {
  const g = S.hostPick;
  const custom = !!g.custom;
  const name = h("input", { type: "text", value: g.name || "", placeholder: "e.g. Space Haven" });
  const ports = h("input", { type: "text", value: g.ports || "", placeholder: "e.g. 7777/udp, 27015/tcp", spellcheck: "false" });
  const portsMsg = h("small", {}, "Separate with commas. A bare number forwards both TCP and UDP (safest if unsure). Ranges: 2456-2457/udp.");
  let pt;
  ports.addEventListener("input", () => {
    clearTimeout(pt);
    pt = setTimeout(async () => {
      if (!ports.value.trim()) return;
      try { const r = await post("/api/ports/parse", { spec: ports.value }); portsMsg.textContent = "✓ " + r.summary; portsMsg.style.color = "var(--ok)"; }
      catch (e) { portsMsg.textContent = e.message; portsMsg.style.color = "var(--bad)"; }
    }, 250);
  });
  const extra = h("input", { type: "text", placeholder: "optional, e.g. 64738 for Mumble voice", spellcheck: "false" });
  const target = h("input", { type: "text", value: "127.0.0.1", spellcheck: "false" });
  const keep = h("input", { type: "checkbox", checked: false });
  const asks = [];
  const askFields = custom ? [] : g.ports.map((p, i) => {
    if (!p.ask) return null;
    const inp = h("input", { type: "number", min: 1, max: 65535, value: p.port });
    asks.push([i, inp]);
    return h("label", { class: "field" }, h("span", {}, p.ask), inp,
      (g.helpers || []).length ? h("small", {}, "Leave as is if unsure: TailLAN updates it automatically when it spots your game.") : null);
  });
  const start = h("button", { class: "btn primary big" }, "Start sharing");
  start.addEventListener("click", () => busy(start, async () => {
    const targets = {};
    for (const [i, inp] of asks) if (inp.value) targets[i] = Number(inp.value);
    await post("/api/host", {
      gameId: custom ? "custom" : g.id,
      customName: custom ? name.value : "",
      customPorts: custom ? ports.value : "",
      extraPorts: extra.value,
      targetIP: target.value,
      targets,
      keepCode: keep.checked,
    });
    S.hostPick = null;
  }));
  const saveBtn = custom ? h("button", { class: "btn", onclick: () => {
    S.edit = { name: name.value, portsSpec: ports.value, isNew: true };
    go("games");
  } }, "Save as a game for next time") : null;

  return h("div", {},
    h("div", { class: "row" }, h("button", { class: "btn ghost", onclick: () => { S.hostPick = null; render(); } }, "← Back")),
    h("h1", {}, custom ? "Host another game" : "Host " + g.name),
    h("p", { class: "sub" }, custom ? "Tell TailLAN which ports the game's server uses. Not sure? Use Tools → Port detector while the game is hosting." : "Ports " + g.portSummary),
    h("div", { class: "grid2" },
      h("section", { class: "card" },
        h("h2", {}, "Before you share"),
        !custom && g.host && g.host.steps ? h("ol", { class: "steps" }, g.host.steps.map((s) => h("li", {}, s))) :
          h("ol", { class: "steps" }, h("li", {}, "Start the game's server, or host a lobby in the game."), h("li", {}, "Fill in the ports, then click Start sharing.")),
        g.notes ? h("div", { class: "note", style: "margin-top:12px" }, g.notes) : null,
        (g.links || []).length ? h("div", { class: "small", style: "margin-top:10px" }, g.links.map((l) => h("div", {}, h("a", { href: l.url, target: "_blank", rel: "noopener" }, l.title + " ↗")))) : null),
      h("section", { class: "card stack" },
        custom ? h("label", { class: "field" }, h("span", {}, "Game name"), name) : null,
        custom ? h("label", { class: "field" }, h("span", {}, "Ports"), ports, portsMsg) : null,
        askFields,
        h("label", { class: "check" }, keep, h("span", {}, h("b", {}, "Keep the same invite code"), h("br"),
          h("span", { class: "small muted" }, "Handy for a regular game night. Anyone you've sent it to can rejoin future sessions, so only use it with friends you trust."))),
        h("details", {},
          h("summary", {}, "Advanced"),
          h("div", { class: "stack" },
            h("label", { class: "field" }, h("span", {}, "Extra ports to share"), extra, h("small", {}, "Shared along with the game, e.g. a voice server.")),
            h("label", { class: "field" }, h("span", {}, "Game server address"), target, h("small", {}, "Keep 127.0.0.1 if the server runs on this computer. Use another device's LAN IP (e.g. 192.168.1.20) if it runs there.")))),
        h("div", { class: "row" }, start, saveBtn))));
}

// ----- live session -----
const openDetails = new Set();
function viewSession() {
  const s = session();
  const el = s.role === "host" ? viewHosting(s) : viewJoined(s);
  el.id = "session";
  // Keep <details> panels open across live refreshes.
  el.querySelectorAll("details").forEach((d, i) => {
    const k = s.role + i;
    d.open = openDetails.has(k);
    d.addEventListener("toggle", () => d.open ? openDetails.add(k) : openDetails.delete(k));
  });
  return el;
}

function pathBadge(p, ms) {
  if (!p || !p.kind) return h("span", { class: "badge pulse" }, h("span", { class: "dot" }), "Finding best route");
  const lat = ms || p.latencyMs;
  const t = lat ? " · " + Math.round(lat) + " ms" : "";
  if (p.kind === "direct") return h("span", { class: "badge ok", title: "Peer-to-peer via " + p.via }, h("span", { class: "dot" }), "Direct" + t);
  return h("span", { class: "badge warn", title: "Your connection is relayed through " + p.via + ". It works, but adds lag. See Tools → Network check." }, h("span", { class: "dot" }), "Relayed (" + p.via + ")" + t);
}

function shareMessage(s) {
  const dl = S.state.downloadURL;
  return [
    `Join my ${s.game.name} game with TailLAN!`,
    dl ? `1. Get TailLAN (free, one small app): ${dl}` : "1. Open TailLAN (free, one small app).",
    "2. Click Join a game and paste this code:",
    "",
    s.invite,
    "",
    "(Keep this code private: anyone with it can join.)",
  ].join("\n");
}

function viewHosting(s) {
  s.friends = s.friends || [];
  const online = s.friends.filter((f) => f.online);
  const groups = h("table", { class: "list" },
    h("thead", {}, h("tr", {}, h("th", {}, "What"), h("th", {}, "Port"), h("th", {}, "Game server"), h("th", {}, "In use"))),
    h("tbody", {}, s.groups.map((g) => h("tr", {},
      h("td", {}, g.label || "Game", " ", h("span", { class: "muted small" }, g.proto.toUpperCase())),
      h("td", { class: "mono" }, g.target === Number(g.ports.split("-")[0]) ? g.ports : g.ports + " → " + g.target,
        g.ask ? h("button", { class: "linkbtn small", style: "margin-left:8px", onclick: () => changeTarget(g) }, "change") : null),
      h("td", {}, listenBadge(g), g.note ? h("div", { class: "small muted" }, g.note) : null),
      h("td", {}, g.active ? g.active + " open" : h("span", { class: "muted" }, "—"))))));
  const anyDown = s.groups.some((g) => g.listening === "no");
  return h("div", {},
    h("div", { class: "hero-status" },
      h("span", { class: "badge ok" }, h("span", { class: "dot" }), "Sharing"),
      h("h1", {}, s.game.name),
      h("span", { class: "spacer" }),
      h("button", { class: "btn danger", onclick: (e) => busy(e.currentTarget, () => post("/api/host/stop")) }, "Stop sharing")),
    h("div", { class: "grid2" },
      h("section", { class: "card stack" },
        h("h2", {}, "1. Send this to your friends"),
        h("div", { class: "invite-box", title: "Invite code" }, s.invite),
        h("div", { class: "row" },
          h("button", { class: "btn primary", onclick: () => copy(shareMessage(s), "Invite message") }, "Copy invite message"),
          h("button", { class: "btn", onclick: () => copy(s.invite, "Code") }, "Copy code only")),
        h("p", { class: "small muted", style: "margin:0" }, "Share it privately (DM, group chat). It's the only key to your game. ",
          s.keepCode ? "This code stays the same next time." : "It stops working when you stop sharing.")),
      h("section", { class: "card stack" },
        h("h2", {}, "2. Start your game"),
        anyDown ? h("div", { class: "note warn" }, "Your game isn't listening yet. Start its server or open your world/lobby; this updates by itself.") : null,
        groups,
        s.targetIP !== "127.0.0.1" ? h("div", { class: "small muted" }, "Forwarding to " + s.targetIP + " on your network.") : null)),
    h("section", { class: "card" },
      h("div", { class: "row" },
        h("h2", { style: "margin:0" }, "Friends ", h("span", { class: "muted" }, "(" + online.length + " here)")),
        h("span", { class: "spacer" }),
        h("label", { class: "check small", title: "Only friends already connected can come back; the invite stops admitting anyone new." },
          h("input", { type: "checkbox", checked: s.locked, onchange: (e) => post("/api/host/lock", { locked: e.target.checked }).catch((er) => toast(er.message, true)) }),
          h("span", {}, "Lock room"))),
      s.friends.length === 0 ? h("div", { class: "empty" }, "Waiting for friends to join... They'll show up here.") :
        h("table", { class: "list", style: "margin-top:10px" },
          h("tbody", {}, s.friends.map((f) => h("tr", {},
            h("td", {}, h("b", {}, f.name), f.app !== "TailLAN" ? h("span", { class: "muted small" }, " (tailcat CLI)") : null),
            h("td", {}, f.online ? pathBadge(f.path) : h("span", { class: "badge" }, "Left")),
            h("td", { class: "small muted" }, f.online && f.since ? "since " + new Date(f.since).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : ""),
            h("td", { style: "text-align:right" }, h("button", { class: "btn small danger", onclick: (e) => {
              const msg = "Remove " + f.name + "? This also locks the room, so only friends who are here now can reconnect." +
                (s.keepCode ? " Your kept invite code still works next time: use Retire invite in the Games tab to keep them out for good." : "");
              if (confirm(msg)) busy(e.currentTarget, () => post("/api/host/kick", { id: f.id }));
            } }, "Remove"))))))),
    h("details", { class: "card" },
      h("summary", {}, "Details for tinkerers"),
      h("dl", { class: "kv" },
        h("dt", {}, "Host"), h("dd", {}, s.hostName),
        h("dt", {}, "Ports"), h("dd", { class: "mono" }, s.game.ports.map((p) => p.port + (p.end ? "-" + p.end : "") + "/" + p.proto).join(", ")),
        h("dt", {}, "tailcat address"), h("dd", { class: "mono", style: "word-break:break-all" }, s.addr)),
      s.tailcatHint ? h("div", { class: "stack", style: "margin-top:12px" },
        h("div", { class: "small muted" }, "Friend can't install TailLAN but has the tailcat CLI? For this TCP-only game they can run:"),
        h("div", { class: "invite-box" }, s.tailcatHint)) : null,
      s.scanError ? h("div", { class: "note warn" }, "Can't check whether the game is running: " + s.scanError) : null));
}

function listenBadge(g) {
  switch (g.listening) {
    case "yes": return h("span", { class: "badge ok", title: g.process ? "Process: " + g.process : null }, h("span", { class: "dot" }), "Running" + (g.process ? " (" + g.process + ")" : ""));
    case "partial": return h("span", { class: "badge warn" }, "Partly running");
    case "no": return h("span", { class: "badge bad pulse" }, h("span", { class: "dot" }), "Not running yet");
    default: return h("span", { class: "badge" }, "Can't tell");
  }
}

async function changeTarget(g) {
  const v = prompt((g.ask || "Port the game listens on") + ":", g.target);
  if (!v) return;
  try { await post("/api/host/target", { group: g.index, port: Number(v) }); }
  catch (e) { toast(e.message, true); }
}

function viewJoined(s) {
  const host = s.hostName || "your friend";
  let banner;
  switch (s.phase) {
    case "connected": banner = h("span", { class: "badge ok" }, h("span", { class: "dot" }), "Connected"); break;
    case "ended": banner = h("span", { class: "badge" }, "Session over"); break;
    case "reconnecting": banner = h("span", { class: "badge warn pulse" }, h("span", { class: "dot" }), "Reconnecting"); break;
    default: banner = h("span", { class: "badge accent pulse" }, h("span", { class: "dot" }), "Connecting");
  }
  if (s.phase === "ended") {
    return h("div", {},
      h("div", { class: "hero-status" }, banner, h("h1", {}, s.game.name)),
      h("section", { class: "card stack" },
        h("p", { style: "margin:0;font-size:17px" }, s.message || "The session has ended."),
        h("div", { class: "row" }, h("button", { class: "btn primary", onclick: (e) => busy(e.currentTarget, () => post("/api/join/stop")) }, "Done"))));
  }
  return h("div", {},
    h("div", { class: "hero-status" },
      banner,
      h("h1", {}, s.game.name),
      h("span", { class: "muted" }, "hosted by " + host),
      h("span", { class: "spacer" }),
      h("button", { class: "btn danger", onclick: (e) => busy(e.currentTarget, () => post("/api/join/stop")) }, "Leave")),
    s.phase !== "connected" && s.message ? h("div", { class: "note warn", style: "margin-bottom:16px" }, s.message) : null,
    h("section", { class: "card stack" },
      h("h2", {}, "In the game, connect to"),
      h("div", { class: "connect-box" },
        h("span", { class: "addr" }, s.connect),
        h("button", { class: "btn", onclick: () => copy(s.connect, "Address") }, "Copy"),
        h("span", { class: "spacer" }),
        s.phase === "connected" ? pathBadge(s.path, s.rttMs) : null),
      h("ol", { class: "steps" }, (s.steps || []).map((x) => h("li", {}, x))),
      (s.notes || []).map((n) => h("div", { class: "note ok" }, "✓ " + n)),
      (s.warnings || []).map((n) => h("div", { class: "note warn" }, n)),
      s.game.notes ? h("div", { class: "note" }, s.game.notes) : null),
    h("details", { class: "card" },
      h("summary", {}, "Port details"),
      h("table", { class: "list" },
        h("thead", {}, h("tr", {}, h("th", {}, "What"), h("th", {}, "On this computer"), h("th", {}, "Host port"), h("th", {}, "In use"))),
        h("tbody", {}, s.ports.map((p) => h("tr", {},
          h("td", {}, (p.label || "Game") + " ", h("span", { class: "muted small" }, p.proto.toUpperCase())),
          h("td", { class: "mono" }, p.local),
          h("td", { class: "mono" }, p.remote),
          h("td", {}, p.active ? p.active + " open" : h("span", { class: "muted" }, "—"))))))));
}

// ===== Games =====
function viewGames() {
  const wrap = h("div", {});
  const q = h("input", { type: "text", placeholder: "Search games...", "aria-label": "Search games" });
  const list = h("div", {});
  const fill = () => {
    const query = q.value.trim().toLowerCase();
    const items = S.games.filter((g) => !query || [g.id, g.name, ...(g.aliases || []), ...(g.tags || [])].some((x) => x.toLowerCase().includes(query)));
    list.replaceChildren(h("table", { class: "list" },
      h("thead", {}, h("tr", {}, h("th", {}, "Game"), h("th", {}, "Ports"), h("th", {}, ""), h("th", {}, ""))),
      h("tbody", {}, items.map((g) => h("tr", {},
        h("td", {}, h("b", {}, g.name), g.source === "user" ? h("span", { class: "badge accent", style: "margin-left:8px" }, "yours") : null,
          g.hasSavedInvite ? h("span", { class: "badge", style: "margin-left:8px", title: "You kept an invite code for this game" }, "saved invite") : null),
        h("td", { class: "mono small" }, g.portSummary),
        h("td", { style: "text-align:right;white-space:nowrap" },
          h("button", { class: "btn small", onclick: () => { S.hostPick = g; go("play"); } }, "Host"), " ",
          h("button", { class: "btn small", onclick: () => { S.edit = editModel(g); renderEditor(); } }, g.source === "user" ? "Edit" : "Copy & edit")),
        h("td", { style: "text-align:right;white-space:nowrap" },
          g.hasSavedInvite ? h("button", { class: "btn small ghost", title: "Retire the kept invite code; old copies stop working", onclick: async () => {
            await post("/api/host/forget", { gameId: g.id }); toast("Saved invite retired"); await loadGames(); fill();
          } }, "Retire invite") : null,
          g.source === "user" ? h("button", { class: "btn small ghost danger", onclick: async () => {
            if (!confirm("Delete your game \"" + g.name + "\"?")) return;
            try { await post("/api/games/delete", { id: g.id }); await loadGames(); fill(); } catch (e) { toast(e.message, true); }
          } }, "Delete") : null))))));
  };
  q.addEventListener("input", fill);
  fill();
  wrap.append(
    h("div", { class: "row" }, h("h1", {}, "Games"), h("span", { class: "spacer" }),
      h("button", { class: "btn", onclick: () => post("/api/open", { which: "games" }).catch((e) => toast(e.message, true)) }, "Open games folder"),
      h("button", { class: "btn primary", onclick: () => { S.edit = { isNew: true }; renderEditor(); } }, "Add a game")),
    h("p", { class: "sub" }, "Each game is a small JSON file listing its ports and how to host and join. Add your own here, by dropping files in your games folder, or with ",
      h("code", {}, "taillan game new"), "."),
    (S.state && S.state.problems || []).map((p) => h("div", { class: "note bad", style: "margin-bottom:10px" }, "Couldn't load " + p)),
    h("div", { id: "editor" }),
    h("section", { class: "card" }, q, h("div", { style: "margin-top:12px" }, list)));
  if (S.edit) setTimeout(renderEditor);
  return wrap;
}

function editModel(g) {
  return {
    isNew: g.source !== "user",
    id: g.source === "user" ? g.id : "",
    name: g.source === "user" ? g.name : g.name + " (custom)",
    portsSpec: g.ports.map((p) => p.port + (p.end ? "-" + p.end : "") + "/" + p.proto).join(", "),
    connect: g.connect || "",
    hostSteps: ((g.host && g.host.steps) || []).join("\n"),
    joinSteps: ((g.join && g.join.steps) || []).join("\n"),
    notes: g.notes || "",
    base: g,
  };
}

function renderEditor() {
  const box = document.getElementById("editor");
  if (!box) return;
  const e = S.edit;
  if (!e) { box.replaceChildren(); return; }
  const f = {
    name: h("input", { type: "text", value: e.name || "", placeholder: "Game name" }),
    ports: h("input", { type: "text", value: e.portsSpec || "", placeholder: "e.g. 7777/udp, 27015/tcp", spellcheck: "false" }),
    connect: h("input", { type: "text", value: e.connect || "", placeholder: "{{host}}:{{port}}", spellcheck: "false" }),
    hostSteps: h("textarea", { rows: 3, placeholder: "One step per line, e.g. Start a server from the main menu" }, e.hostSteps || ""),
    joinSteps: h("textarea", { rows: 3, placeholder: "One step per line, e.g. Multiplayer → Direct connect → {{connect}}" }, e.joinSteps || ""),
    notes: h("textarea", { rows: 2, placeholder: "Caveats, e.g. turn off crossplay" }, e.notes || ""),
  };
  const save = h("button", { class: "btn primary" }, "Save game");
  save.addEventListener("click", () => busy(save, async () => {
    const base = e.base || {};
    const prof = {
      id: e.id || "",
      name: f.name.value.trim(),
      aliases: base.aliases, links: base.links, tags: base.tags, helpers: base.helpers,
      connect: f.connect.value.trim(),
      notes: f.notes.value.trim(),
      host: {}, join: {},
    };
    // Keep per-port extras (labels, ask, lan...) when ports are unchanged.
    const same = e.base && f.ports.value === e.portsSpec;
    if (same) prof.ports = e.base.ports;
    const r = await post("/api/games", { profile: prof, portsSpec: same ? "" : f.ports.value, hostSteps: f.hostSteps.value || " ", joinSteps: f.joinSteps.value || " " });
    toast("Saved " + prof.name);
    S.edit = null;
    await loadGames();
    render();
    return r;
  }));
  box.replaceChildren(h("section", { class: "card stack", style: "margin-bottom:18px" },
    h("h2", {}, e.isNew ? "Add a game" : "Edit " + e.name),
    h("div", { class: "grid2" },
      h("label", { class: "field" }, h("span", {}, "Name"), f.name),
      h("label", { class: "field" }, h("span", {}, "Ports"), f.ports, h("small", {}, "Bare number = TCP+UDP. Not sure? Tools → Port detector."))),
    h("label", { class: "field" }, h("span", {}, "What friends type in the game"), f.connect,
      h("small", {}, "{{host}} is 127.0.0.1 and {{port}} the first port (it may change if busy on their machine). Examples: {{host}}:{{port}}, connect {{host}}:{{port}}")),
    h("div", { class: "grid2" },
      h("label", { class: "field" }, h("span", {}, "Steps for the host"), f.hostSteps),
      h("label", { class: "field" }, h("span", {}, "Steps for friends joining"), f.joinSteps, h("small", {}, "You can use {{connect}}, {{host}}, {{port}}, {{hostname}}."))),
    h("label", { class: "field" }, h("span", {}, "Notes"), f.notes),
    h("div", { class: "row" }, save, h("button", { class: "btn ghost", onclick: () => { S.edit = null; renderEditor(); } }, "Cancel"),
      h("span", { class: "spacer" }), h("span", { class: "small muted" }, "Saved to " + ((S.state && S.state.gamesDir) || "your games folder")))));
  box.scrollIntoView({ behavior: "smooth", block: "start" });
}

// ===== Tools =====
function viewTools() {
  return h("div", {},
    h("h1", {}, "Tools"),
    h("p", { class: "sub" }, "Figure out an unknown game's ports, check your network, and see what's happening."),
    cardDetector(), cardDoctor(), cardLogs());
}

function cardDetector() {
  const body = h("div", { id: "detect" });
  const scan = async (btn) => busy(btn, async () => {
    S.detect = await get("/api/detect");
    body.replaceChildren(detectTable());
  });
  const snap = h("button", { class: "btn", onclick: (e) => busy(e.currentTarget, async () => {
    S.detectBase = await get("/api/detect");
    S.detect = null;
    toast("Snapshot taken. Now start your game's server, then click Scan.");
    body.replaceChildren(detectTable());
  }) }, "1. Snapshot (before starting the game)");
  const scanBtn = h("button", { class: "btn primary" }, "Scan now");
  scanBtn.addEventListener("click", () => scan(scanBtn));
  body.append(detectTable());
  return h("section", { class: "card stack" },
    h("h2", {}, "Port detector"),
    h("p", { class: "muted", style: "margin:0" }, "Start hosting in your game (server running, lobby open), then scan to see which ports it opened. For a clean result, snapshot first: new ports get highlighted."),
    h("div", { class: "row" }, snap, scanBtn),
    body);
}

function detectTable() {
  if (!S.detect) return h("div", { class: "empty" }, S.detectBase ? "Snapshot saved. Start the game's server, then click Scan now." : "Click Scan now to list programs that are waiting for connections.");
  const base = new Set();
  for (const p of S.detectBase || []) for (const s of p.sockets) base.add(s.proto + s.port + "|" + p.pid);
  const rows = [];
  for (const p of S.detect) {
    if (p.self) continue;
    const fresh = p.sockets.filter((s) => !base.has(s.proto + s.port + "|" + p.pid));
    if (S.detectBase && fresh.length === 0) continue;
    const spec = S.detectBase ? specOf(fresh) : p.spec;
    rows.push(h("tr", { class: S.detectBase ? "new" : "" },
      h("td", {}, h("b", {}, p.name), h("div", { class: "small muted" }, "PID " + p.pid)),
      h("td", { class: "mono small" }, spec),
      h("td", { style: "text-align:right;white-space:nowrap" },
        h("button", { class: "btn small", onclick: () => { S.hostPick = { custom: true, name: cleanName(p.name), ports: spec }; go("play"); } }, "Share these"), " ",
        h("button", { class: "btn small", onclick: () => { S.edit = { isNew: true, name: cleanName(p.name), portsSpec: spec }; go("games"); } }, "Save as game"))));
  }
  if (!rows.length) return h("div", { class: "empty" }, "No new ports since the snapshot. Is the server fully started? Some games only open ports once a lobby exists.");
  return h("table", { class: "list" },
    h("thead", {}, h("tr", {}, h("th", {}, "Program"), h("th", {}, S.detectBase ? "New ports" : "Ports"), h("th", {}, ""))),
    h("tbody", {}, rows));
}
function cleanName(n) { return n.replace(/\.exe$/i, "").replace(/[-_]?(server|dedicated|ds)$/i, "").replace(/^./, (c) => c.toUpperCase()); }
function specOf(socks) {
  const m = new Map();
  for (const s of socks) { const set = m.get(s.port) || new Set(); set.add(s.proto); m.set(s.port, set); }
  return [...m.keys()].sort((a, b) => a - b).map((p) => { const s = m.get(p); return p + "/" + (s.size > 1 ? "both" : [...s][0]); }).join(", ");
}

function cardDoctor() {
  const out = h("div", {}, doctorView());
  const btn = h("button", { class: "btn primary" }, S.doctor ? "Run again" : "Run checks");
  btn.addEventListener("click", () => busy(btn, async () => {
    S.doctor = await get("/api/doctor");
    out.replaceChildren(doctorView());
  }));
  return h("section", { class: "card stack" },
    h("h2", {}, "Network check"),
    h("p", { class: "muted", style: "margin:0" }, "Checks the relays, UDP, your router's NAT type, and runs a full test connection. Takes about ten seconds."),
    h("div", { class: "row" }, btn), out);
}
function doctorView() {
  const r = S.doctor;
  if (!r) return h("div");
  const icon = { ok: "✓", warn: "!", fail: "✗" };
  return h("div", {},
    h("div", { class: "note " + (r.checks.some((c) => c.status === "fail") ? "bad" : r.checks.some((c) => c.status === "warn") ? "warn" : "ok") }, r.summary),
    h("ul", { class: "check-list" }, r.checks.map((c) => h("li", {},
      h("span", { class: "icon " + c.status }, icon[c.status]),
      h("div", {}, h("b", {}, c.name), h("div", { class: "small muted" }, c.detail))))));
}

function cardLogs() {
  const logs = h("div", { id: "logs", class: "logs" }, "Loading...");
  updateLogs(logs);
  const verbose = h("input", { type: "checkbox", checked: S.state && S.state.verbose, onchange: async (e) => {
    try { await post("/api/settings", { verbose: e.target.checked }); toast("Applies to the next session"); } catch (er) { toast(er.message, true); }
  } });
  return h("section", { class: "card stack" },
    h("div", { class: "row" }, h("h2", { style: "margin:0" }, "Activity log"), h("span", { class: "spacer" }),
      h("label", { class: "check small" }, verbose, h("span", {}, "Detailed tunnel logs")),
      h("button", { class: "btn small", onclick: async () => copy((await get("/api/logs")).join("\n"), "Log") }, "Copy")),
    logs);
}
let logBusy = false;
async function updateLogs(el) {
  if (logBusy) return;
  logBusy = true;
  try {
    const lines = (await get("/api/logs")) || [];
    const atEnd = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
    el.textContent = lines.length ? lines.join("\n") : "Nothing yet.";
    if (atEnd) el.scrollTop = el.scrollHeight;
  } catch { /* offline */ }
  logBusy = false;
}

// ===== Help =====
function viewHelp() {
  return h("div", {},
    h("h1", {}, "How it works"),
    h("p", { class: "sub" }, "TailLAN makes your friend's game server feel like it's running on your own computer."),
    h("section", { class: "card" }, h("div", { class: "how" },
      h("div", {}, h("div", { class: "n" }, "1"), h("b", {}, "The host shares"), h("p", { class: "muted small" }, "TailLAN opens an encrypted tunnel (tailcat, built on Tailscale's WireGuard tech) and gives you an invite code. Nothing on your router changes.")),
      h("div", {}, h("div", { class: "n" }, "2"), h("b", {}, "Friends paste the code"), h("p", { class: "muted small" }, "Their TailLAN connects to yours, directly peer-to-peer when possible, through a relay when not, and opens the game's ports on their computer.")),
      h("div", {}, h("div", { class: "n" }, "3"), h("b", {}, "Everyone connects to 127.0.0.1"), h("p", { class: "muted small" }, "In the game, friends connect to their own computer. TailLAN carries the traffic to the host.")))),
    h("section", { class: "card" },
      h("h2", {}, "Questions"),
      h("dl", { class: "faq" },
        h("dt", {}, "Do my friends need TailLAN?"),
        h("dd", {}, "Yes, everyone runs it (it's one small app, no install or account). Just the host needs the game server running."),
        h("dt", {}, "Is it safe?"),
        h("dd", {}, "Traffic is end-to-end encrypted with WireGuard. Only the ports of the game you picked are reachable, nothing else on your computer or network. The invite code is the key: share it privately, and use Lock room once everyone's in. Codes stop working when you stop sharing (unless you chose to keep one)."),
        h("dt", {}, "My game isn't in the list."),
        h("dd", {}, "Choose Another game and enter its ports. Don't know them? Start hosting in the game, then use Tools → Port detector. Save it as a game so it's one click next time, and consider contributing the file so everyone gets it."),
        h("dt", {}, "It says Relayed. Is that bad?"),
        h("dd", {}, "It still works, but traffic takes a detour through a shared relay server, adding lag and limiting speed. It usually happens when both sides have strict routers or UDP is blocked. Tools → Network check tells you more."),
        h("dt", {}, "The game connected, then nothing happens."),
        h("dd", {}, "Check the host page says the game server is Running, and that everyone has the same game version. Some games need a second port (query, voice); add it under Advanced → Extra ports."),
        h("dt", {}, "What won't work?"),
        h("dd", {}, "Games that only use Steam/Epic/console matchmaking (use their own invites instead), games that only find each other by LAN broadcast with no way to type an address, and games where every player connects to every other player (some old RTS games). TailLAN forwards ports to one host; it isn't a full virtual LAN."))),
    h("section", { class: "card" },
      h("h2", {}, "About"),
      h("p", { class: "muted", style: "margin:0" }, "TailLAN " + ((S.state && S.state.version) || "") + ". Built on ",
        h("a", { href: "https://github.com/tailscale/tailcat", target: "_blank", rel: "noopener" }, "tailcat"),
        ". Your settings and games live in ", h("code", {}, (S.state && S.state.gamesDir || "").replace(/[\\/]games$/, "")), "."),
      h("div", { class: "row", style: "margin-top:12px" },
        h("button", { class: "btn danger", onclick: async () => {
          if (!confirm("Quit TailLAN? Any game you're sharing or playing stops.")) return;
          await post("/api/quit"); document.body.replaceChildren(h("div", { class: "empty", style: "padding:80px" }, "TailLAN has quit. You can close this window."));
        } }, "Quit TailLAN"))));
}

// ---------- boot ----------
(async () => {
  try {
    S.state = await get("/api/state");
    await loadGames();
  } catch (e) {
    main.replaceChildren(h("div", { class: "note bad" }, e.message));
    return;
  }
  renderWho();
  const v = location.hash.slice(1);
  if (["play", "games", "tools", "help"].includes(v)) go(v); else render();
  connect();
})();

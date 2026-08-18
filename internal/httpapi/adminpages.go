package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net/http"
)

// assetCacheControl is as aggressive as it is only because the paths carry a content digest: a
// stale copy can never be served against markup that did not ship with it.
const assetCacheControl = "public, max-age=31536000, immutable"

type setupPageData struct {
	CSRFToken string
	Email     string
	Error     string
}

type loginPageData struct {
	CSRFToken string
	Email     string
	Error     string
	Message   string
}

type adminDeviceView struct {
	ID         string
	Name       string
	Platform   string
	CreatedAt  string
	LastSeenAt string
	RevokedAt  string
	Active     bool
}

// adminNotebookView is one notebook name on the dashboard. Ids are deliberately absent: they are
// client-generated opaque strings that mean nothing to the person reading the list, and the
// timestamp is what actually tells two notebooks with the same name apart.
type adminNotebookView struct {
	Name      string
	UpdatedAt string
}

type dashboardPageData struct {
	Email               string
	CreatedAt           string
	Storage             string
	ActiveDeviceCount   int
	DeviceCount         int
	RevokedDeviceCount  int
	Devices             []adminDeviceView
	NotebookCount       int64
	Notebooks           []adminNotebookView
	HiddenNotebookCount int64
	NotebooksSelected   bool
	CSRFToken           string
}

type adminErrorPageData struct {
	Title   string
	Message string
}

var (
	setupTemplate          = template.Must(template.New("setup").Parse(setupPageHTML))
	loginTemplate          = template.Must(template.New("login").Parse(loginPageHTML))
	adminDashboardTemplate = template.Must(template.New("dashboard").Parse(dashboardPageHTML))
	adminErrorTemplate     = template.Must(template.New("error").Parse(errorPageHTML))
)

func (application adminApplication) renderSetup(w http.ResponseWriter, statusCode int, data setupPageData) {
	application.writeAdminPage(w, statusCode, setupTemplate, data)
}

func (application adminApplication) renderLogin(w http.ResponseWriter, statusCode int, data loginPageData) {
	application.writeAdminPage(w, statusCode, loginTemplate, data)
}

func (application adminApplication) writeAdminPage(w http.ResponseWriter, statusCode int, pageTemplate *template.Template, data any) {
	var body bytes.Buffer
	if err := pageTemplate.Execute(&body, data); err != nil {
		application.logger.Error("rendering an admin page failed", slog.Any("error", err))
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statusCode)
	if _, err := w.Write(body.Bytes()); err != nil {
		application.logger.Debug("writing an admin page failed", "error", err)
	}
}

func (application adminApplication) handleStylesheet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", assetCacheControl)
	_, _ = w.Write([]byte(adminStylesheet))
}

func (application adminApplication) handleAdminScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", assetCacheControl)
	_, _ = w.Write([]byte(adminScript))
}

// The admin assets are served under a path containing a digest of what they hold.
//
// A fixed path plus a cache lifetime is a trap for an upgradeable product: the pages are no-store,
// so a rebuilt server sends new markup immediately, while the browser keeps answering the old
// stylesheet out of cache. New class names against an old stylesheet is not a subtle degradation --
// it is an unstyled page, and it lasts until the cache entry expires. The digest makes that
// impossible: changed content is a changed URL, which cannot be in any cache yet.
var (
	adminStylesheetPath = contentAddressedPath("admin", "css", adminStylesheet)
	adminScriptPath     = contentAddressedPath("admin", "js", adminScript)
)

func contentAddressedPath(name string, extension string, content string) string {
	digest := sha256.Sum256([]byte(content))
	return "/assets/" + name + "." + hex.EncodeToString(digest[:6]) + "." + extension
}

var pageHead = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark">
  <link rel="stylesheet" href="` + adminStylesheetPath + `">
`

var setupPageHTML = pageHead + `<title>Set up viveCServer</title>
</head>
<body class="centered-page">
  <main class="auth-shell">
    <div class="brand-mark" aria-hidden="true">V</div>
    <p class="eyebrow">viveCServer</p>
    <h1>Make this server yours</h1>
    <p class="lede">Create the owner account. Setup closes permanently after this step.</p>
    {{if .Error}}<div class="notice error" role="alert">{{.Error}}</div>{{end}}
    <form method="post" action="/setup" class="stacked-form">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <label>Email
        <input type="email" name="email" value="{{.Email}}" autocomplete="email" required autofocus>
      </label>
      <label>Password
        <input type="password" name="password" minlength="8" maxlength="1024" autocomplete="new-password" required>
        <span class="field-hint">At least 8 characters. Stored as an Argon2id hash.</span>
      </label>
      <label>Confirm password
        <input type="password" name="password_confirmation" minlength="8" maxlength="1024" autocomplete="new-password" required>
      </label>
      <button type="submit">Complete setup</button>
    </form>
    <p class="security-note"><span aria-hidden="true">●</span> Your notes remain opaque to this server.</p>
  </main>
</body>
</html>`

var loginPageHTML = pageHead + `<title>Sign in · viveCServer</title>
</head>
<body class="centered-page">
  <main class="auth-shell">
    <div class="brand-mark" aria-hidden="true">V</div>
    <p class="eyebrow">viveCServer admin</p>
    <h1>Welcome back</h1>
    <p class="lede">Sign in to inspect this server and manage registered devices.</p>
    {{if .Message}}<div class="notice success" role="status">{{.Message}}</div>{{end}}
    {{if .Error}}<div class="notice error" role="alert">{{.Error}}</div>{{end}}
    <form method="post" action="/login" class="stacked-form">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <label>Email
        <input type="email" name="email" value="{{.Email}}" autocomplete="email" required autofocus>
      </label>
      <label>Password
        <input type="password" name="password" autocomplete="current-password" required>
      </label>
      <button type="submit">Sign in</button>
    </form>
  </main>
</body>
</html>`

var dashboardPageHTML = pageHead + `<title>Admin · viveCServer</title>
  <script src="` + adminScriptPath + `" defer></script>
</head>
<body>
  <header class="topbar">
    <a class="brand" href="/admin"><span class="brand-mark small" aria-hidden="true">V</span><span>viveCServer</span></a>
    <form method="post" action="/logout">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <button type="submit" class="button-secondary">Sign out</button>
    </form>
  </header>
  <main class="dashboard">
    <section class="hero">
      <div>
        <p class="eyebrow">Server overview</p>
        <h1>Everything is running.</h1>
        <p class="lede">This server is configured for <strong>{{.Email}}</strong>.</p>
      </div>
      <span class="status-pill"><span></span> Online</span>
    </section>
    <section class="metric-grid" aria-label="Server statistics">
      <article class="metric"><span>Active devices</span><strong>{{.ActiveDeviceCount}}</strong><small>{{.DeviceCount}} total</small></article>
      <article class="metric"><span>Synced storage</span><strong>{{.Storage}}</strong><small>Attachment blobs</small></article>
      <article class="metric"><span>Account created</span><strong class="metric-date">{{.CreatedAt}}</strong><small>Owner account</small></article>
    </section>
    <section class="panel" id="contents">
      <div class="panel-heading tabbed">
        <!-- Real links, not scripted buttons. Without the script they still work: the server reads
             ?tab= and renders the same page with the other panel showing. -->
        <nav class="tab-strip" data-tab-strip aria-label="Server contents">
          <a class="tab{{if not .NotebooksSelected}} is-selected{{end}}" href="/admin?tab=devices" data-tab="devices"{{if not .NotebooksSelected}} aria-current="page"{{end}}>Devices <span class="count-badge">{{.DeviceCount}}</span></a>
          <a class="tab{{if .NotebooksSelected}} is-selected{{end}}" href="/admin?tab=notebooks" data-tab="notebooks"{{if .NotebooksSelected}} aria-current="page"{{end}}>Notebooks <span class="count-badge">{{.NotebookCount}}</span></a>
        </nav>
        <div class="panel-actions" data-panel="devices"{{if .NotebooksSelected}} hidden{{end}}>
          {{if .RevokedDeviceCount}}
          <form method="post" action="/admin/devices/remove-revoked">
            <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
            <button type="submit" class="button-secondary button-small">Remove {{.RevokedDeviceCount}} revoked</button>
          </form>
          {{end}}
        </div>
      </div>
      <div data-panel="devices"{{if .NotebooksSelected}} hidden{{end}}>
      {{if .Devices}}
      <div class="record-list">
        {{range .Devices}}
        <article class="record-row" id="device-{{.ID}}">
          <div class="record-icon" aria-hidden="true">◆</div>
          <div class="record-main">
            <div class="record-title"><strong>{{.Name}}</strong>{{if .Active}}<span class="record-state active">Active</span>{{else}}<span class="record-state">Revoked</span>{{end}}</div>
            <p>{{if .Platform}}{{.Platform}} · {{end}}Last seen {{.LastSeenAt}}</p>
            <small>Registered {{.CreatedAt}}{{if .RevokedAt}} · Revoked {{.RevokedAt}}{{end}}</small>
          </div>
          {{if .Active}}
          <form method="post" action="/admin/devices/{{.ID}}/rename" class="device-rename">
            <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
            <label class="visually-hidden" for="rename-{{.ID}}">Name for this device</label>
            <input id="rename-{{.ID}}" type="text" name="name" value="{{.Name}}" maxlength="128" required>
            <button type="submit">Rename</button>
          </form>
          <form method="post" action="/admin/devices/{{.ID}}/revoke">
            <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
            <button type="submit" class="button-danger">Revoke</button>
          </form>
          {{else}}
          <form method="post" action="/admin/devices/{{.ID}}/remove">
            <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
            <button type="submit" class="button-danger">Remove</button>
          </form>
          {{end}}
        </article>
        {{end}}
      </div>
      {{else}}
      <div class="empty-state"><span aria-hidden="true">◇</span><h3>No devices yet</h3><p>Connect viveNotes to the sync port to register the first device.</p></div>
      {{end}}
      </div>
      <div data-panel="notebooks"{{if not .NotebooksSelected}} hidden{{end}}>
      {{if .Notebooks}}
      <div class="record-list">
        {{range .Notebooks}}
        <article class="record-row">
          <div class="record-icon" aria-hidden="true">▤</div>
          <div class="record-main">
            <div class="record-title"><strong>{{.Name}}</strong></div>
            <p>Last changed {{.UpdatedAt}}</p>
          </div>
        </article>
        {{end}}
      </div>
      {{if .HiddenNotebookCount}}<p class="list-note">{{.HiddenNotebookCount}} more not shown.</p>{{end}}
      {{else}}
      <div class="empty-state"><span aria-hidden="true">◇</span><h3>No notebooks yet</h3><p>Notebooks appear here once a device syncs them to this server.</p></div>
      {{end}}
      </div>
    </section>
    <section class="panel live-log-panel" aria-labelledby="live-log-title">
      <div class="panel-heading">
        <div><p class="eyebrow">Diagnostics</p><h2 id="live-log-title">Live server logs</h2></div>
        <div class="log-actions">
          <span id="live-log-status" class="log-status connecting"><span></span> Connecting</span>
          <button id="clear-live-logs" type="button" class="button-secondary button-small">Clear</button>
        </div>
      </div>
      <div id="live-log-output" class="log-output" role="log" aria-live="polite" aria-relevant="additions">
        <p class="log-empty">Waiting for new server events…</p>
      </div>
      <p class="log-note">Only new events are shown. Logs are not stored by the admin panel and disappear when this page is reloaded.</p>
    </section>
    <p class="footer-note">Admin interface on port 8080 · Device sync API on port 8281</p>
  </main>
</body>
</html>`

var errorPageHTML = pageHead + `<title>{{.Title}} · viveCServer</title>
</head>
<body class="centered-page">
  <main class="auth-shell compact">
    <div class="brand-mark" aria-hidden="true">!</div>
    <p class="eyebrow">viveCServer</p>
    <h1>{{.Title}}</h1>
    <p class="lede">{{.Message}}</p>
    <a class="button-link" href="/">Return home</a>
  </main>
</body>
</html>`

const adminStylesheet = `
:root {
  color-scheme: dark;
  --background: #090c0b;
  --surface: #111613;
  --surface-raised: #171e1a;
  --border: #27332c;
  --text: #f3f7f4;
  --muted: #9baaa0;
  --accent: #72e6a5;
  --accent-dark: #07150d;
  --danger: #ff8e8e;
  --danger-bg: #2b1517;
  --success-bg: #10291c;
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; color: var(--text); background: radial-gradient(circle at 50% -20%, #183124 0, var(--background) 40rem); }
button, input { font: inherit; }
.centered-page { display: grid; place-items: center; padding: 2rem 1rem; }
.auth-shell { width: min(100%, 29rem); padding: 2.25rem; border: 1px solid var(--border); border-radius: 1.25rem; background: color-mix(in srgb, var(--surface) 94%, transparent); box-shadow: 0 1.5rem 5rem #0008; }
.auth-shell.compact { text-align: center; }
.brand-mark { display: grid; width: 3rem; height: 3rem; place-items: center; margin-bottom: 1.25rem; border-radius: .9rem; color: var(--accent-dark); background: var(--accent); font-weight: 900; letter-spacing: -.08em; }
.brand-mark.small { width: 2rem; height: 2rem; margin: 0; border-radius: .6rem; font-size: .85rem; }
.eyebrow { margin: 0 0 .5rem; color: var(--accent); font-size: .74rem; font-weight: 800; letter-spacing: .14em; text-transform: uppercase; }
h1 { margin: 0; font-size: clamp(2rem, 7vw, 3rem); line-height: 1.04; letter-spacing: -.045em; }
h2 { margin: .15rem 0 0; font-size: 1.45rem; letter-spacing: -.025em; }
h3 { margin: .75rem 0 .25rem; }
.lede { margin: 1rem 0 1.75rem; color: var(--muted); line-height: 1.6; }
.lede strong { color: var(--text); }
.stacked-form { display: grid; gap: 1.1rem; }
label { display: grid; gap: .48rem; color: #dce5df; font-size: .9rem; font-weight: 700; }
input { width: 100%; padding: .85rem .95rem; border: 1px solid #35443a; border-radius: .7rem; outline: none; color: var(--text); background: #0b0f0d; transition: border-color .15s, box-shadow .15s; }
input:focus { border-color: var(--accent); box-shadow: 0 0 0 3px #72e6a522; }
.field-hint { color: var(--muted); font-size: .76rem; font-weight: 450; }
button, .button-link { display: inline-flex; align-items: center; justify-content: center; min-height: 2.7rem; padding: .7rem 1rem; border: 0; border-radius: .7rem; color: var(--accent-dark); background: var(--accent); font-weight: 800; text-decoration: none; cursor: pointer; }
button:hover, .button-link:hover { filter: brightness(1.08); }
.button-secondary { min-height: 2.35rem; border: 1px solid var(--border); color: var(--text); background: var(--surface-raised); }
.button-danger { min-height: 2.35rem; border: 1px solid #653338; color: var(--danger); background: transparent; }
.security-note { margin: 1.5rem 0 0; color: var(--muted); font-size: .78rem; text-align: center; }
.security-note span { color: var(--accent); }
.notice { margin: 0 0 1.25rem; padding: .8rem .9rem; border: 1px solid; border-radius: .7rem; font-size: .86rem; line-height: 1.45; }
.notice.error { border-color: #613338; color: #ffc0c0; background: var(--danger-bg); }
.notice.success { border-color: #295f40; color: #b9f8d2; background: var(--success-bg); }
.topbar { display: flex; align-items: center; justify-content: space-between; min-height: 4.5rem; padding: .75rem max(1rem, calc((100vw - 70rem) / 2)); border-bottom: 1px solid var(--border); background: #090c0bdd; backdrop-filter: blur(1rem); }
.brand { display: flex; align-items: center; gap: .7rem; color: var(--text); font-weight: 850; text-decoration: none; }
.dashboard { width: min(100% - 2rem, 70rem); margin: 0 auto; padding: 4.5rem 0 2rem; }
.hero { display: flex; align-items: flex-start; justify-content: space-between; gap: 2rem; }
.hero .lede { margin-bottom: 0; }
.status-pill { display: inline-flex; align-items: center; gap: .5rem; padding: .5rem .75rem; border: 1px solid #295f40; border-radius: 999px; color: #b9f8d2; background: var(--success-bg); font-size: .8rem; font-weight: 800; white-space: nowrap; }
.status-pill span { width: .48rem; height: .48rem; border-radius: 50%; background: var(--accent); box-shadow: 0 0 .7rem var(--accent); }
.metric-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 1rem; margin: 2.5rem 0; }
.metric { display: grid; gap: .4rem; min-height: 9rem; padding: 1.25rem; border: 1px solid var(--border); border-radius: 1rem; background: linear-gradient(145deg, var(--surface-raised), var(--surface)); }
.metric span, .metric small { color: var(--muted); font-size: .78rem; }
.metric strong { margin-top: auto; font-size: 2rem; letter-spacing: -.04em; }
.metric strong.metric-date { font-size: 1.05rem; letter-spacing: -.015em; }
.list-note { margin: 0; padding: 0 1.35rem 1.2rem; color: var(--muted); font-size: .76rem; }
.panel-actions { display: flex; align-items: center; gap: .75rem; }
/* The tab strip is the panel's title: "Devices" and "Notebooks" name their own contents better than
   a heading above them could, so the heading loses its eyebrow and h2 and carries these instead. */
.panel-heading.tabbed { padding: .8rem 1rem .8rem .9rem; }
.tab-strip { display: flex; gap: .3rem; }
.tab { display: inline-flex; align-items: center; gap: .55rem; padding: .5rem .8rem; border: 1px solid transparent; border-radius: .75rem; color: var(--muted); font-size: 1rem; font-weight: 800; letter-spacing: -.02em; text-decoration: none; }
.tab:hover { color: var(--text); background: #ffffff06; }
.tab.is-selected { border-color: var(--border); color: var(--text); background: var(--surface-raised); }
.tab .count-badge { min-width: 1.7rem; height: 1.7rem; color: var(--muted); background: #ffffff0a; font-size: .72rem; }
.tab.is-selected .count-badge { color: var(--accent); background: #72e6a513; }
/* A class that sets a display value outranks the browser's own [hidden] rule, and the panel actions
   carry one. Without this, hiding them on the notebooks tab would not hide them. */
[hidden] { display: none !important; }
.panel { overflow: hidden; border: 1px solid var(--border); border-radius: 1rem; background: var(--surface); }
.panel + .panel { margin-top: 1rem; }
.panel-heading { display: flex; align-items: center; justify-content: space-between; padding: 1.35rem; border-bottom: 1px solid var(--border); }
.count-badge { display: grid; min-width: 2rem; height: 2rem; place-items: center; border-radius: 999px; color: var(--accent); background: #72e6a513; font-size: .8rem; font-weight: 850; }
/* One row shape for both tabs. A notebook and a device are different things, but a list of them is
   the same list, and switching tabs should not feel like switching pages. */
.record-list { display: grid; }
.record-row { display: flex; align-items: center; gap: 1rem; padding: 1.15rem 1.35rem; }
.record-row + .record-row { border-top: 1px solid var(--border); }
.record-icon { display: grid; flex: 0 0 2.5rem; height: 2.5rem; place-items: center; border-radius: .7rem; color: var(--accent); background: #72e6a510; }
.record-main { min-width: 0; flex: 1; }
.record-title { display: flex; align-items: center; gap: .65rem; }
.record-title strong { overflow-wrap: anywhere; }
.record-main p, .record-main small { margin: .25rem 0 0; color: var(--muted); font-size: .8rem; }
.record-state { padding: .15rem .45rem; border-radius: 999px; color: var(--muted); background: #ffffff0a; font-size: .65rem; font-weight: 800; text-transform: uppercase; }
.record-state.active { color: var(--accent); background: #72e6a510; }
/* The rename field sizes to its own content rather than taking the shared full-width input rule, so
   a device row stays a row: the point of renaming is to tell two rows apart at a glance. */
.device-rename { display: flex; align-items: center; gap: .5rem; }
.device-rename input { width: 12rem; min-height: 2.35rem; padding: .45rem .7rem; font-size: .85rem; }
.device-rename button { min-height: 2.35rem; border: 1px solid var(--border); color: var(--text); background: var(--surface-raised); }
.visually-hidden { position: absolute; width: 1px; height: 1px; padding: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
.empty-state { padding: 4rem 1rem; color: var(--muted); text-align: center; }
.empty-state > span { color: var(--accent); font-size: 2rem; }
.empty-state p { margin: 0; }
.log-actions { display: flex; align-items: center; gap: .75rem; }
.button-small { min-height: 2rem; padding: .4rem .7rem; font-size: .75rem; }
.log-status { display: inline-flex; align-items: center; gap: .45rem; color: var(--muted); font-size: .74rem; font-weight: 800; }
.log-status span { width: .45rem; height: .45rem; border-radius: 50%; background: #e9b949; }
.log-status.live { color: #b9f8d2; }
.log-status.live span { background: var(--accent); box-shadow: 0 0 .55rem var(--accent); }
.log-status.reconnecting { color: #ffd98a; }
.log-output { min-height: 15rem; max-height: 28rem; overflow: auto; padding: .8rem 1.35rem; background: #080b09; font-family: ui-monospace, SFMono-Regular, Consolas, "Liberation Mono", monospace; font-size: .76rem; line-height: 1.55; }
.log-empty { margin: 1rem 0; color: var(--muted); text-align: center; }
.log-row { display: grid; grid-template-columns: 12rem 4rem minmax(0, 1fr); gap: .7rem; padding: .3rem 0; border-bottom: 1px solid #ffffff08; }
.log-time, .log-attributes { color: #76847b; }
.log-level { color: #9aa9ff; font-weight: 800; text-transform: uppercase; }
.log-level.warn { color: #ffd36e; }
.log-level.error { color: var(--danger); }
.log-level.debug { color: #8f9a93; }
.log-message { min-width: 0; overflow-wrap: anywhere; color: #dce5df; }
.log-attributes { grid-column: 3; overflow-wrap: anywhere; }
.log-note { margin: 0; padding: .75rem 1.35rem; border-top: 1px solid var(--border); color: var(--muted); font-size: .72rem; }
.footer-note { margin: 1rem 0; color: #6f7d74; font-size: .75rem; text-align: center; }
@media (max-width: 720px) {
  .auth-shell { padding: 1.5rem; }
  .dashboard { padding-top: 2.5rem; }
  .hero { display: grid; }
  .metric-grid { grid-template-columns: 1fr; }
  .metric { min-height: 7rem; }
  .panel-heading.tabbed { align-items: stretch; flex-direction: column; gap: .75rem; }
  .tab { flex: 1; justify-content: center; }
  .panel-actions { align-items: stretch; flex-direction: column; }
  .panel-actions button { width: 100%; }
  .record-row { align-items: flex-start; flex-wrap: wrap; }
  .record-row form { width: 100%; padding-left: 3.5rem; }
  .record-row button { width: 100%; }
  .live-log-panel .panel-heading { align-items: flex-start; gap: 1rem; }
  .log-actions { align-items: flex-end; flex-direction: column; }
  .log-row { grid-template-columns: 1fr 4rem; gap: .25rem .7rem; }
  .log-time { grid-column: 1; }
  .log-level { grid-column: 2; grid-row: 1; text-align: right; }
  .log-message, .log-attributes { grid-column: 1 / -1; }
}
`

const adminScript = `
(() => {
  "use strict";

  // Tabs. The links work without this -- the server reads ?tab= and renders the same page with the
  // other panel showing -- so all this adds is doing it without a round trip, which also means the
  // page does not jump back to the top on every switch.
  const tabStrip = document.querySelector("[data-tab-strip]");
  if (tabStrip) {
    const tabs = Array.from(tabStrip.querySelectorAll("[data-tab]"));
    const panels = Array.from(document.querySelectorAll("[data-panel]"));

    function showTab(name) {
      for (const tab of tabs) {
        const selected = tab.dataset.tab === name;
        tab.classList.toggle("is-selected", selected);
        if (selected) {
          tab.setAttribute("aria-current", "page");
        } else {
          tab.removeAttribute("aria-current");
        }
      }
      for (const panel of panels) {
        panel.hidden = panel.dataset.panel !== name;
      }
    }

    for (const tab of tabs) {
      tab.addEventListener("click", (event) => {
        // Leave a modified click alone: it is somebody opening the tab in a new window.
        if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
          return;
        }
        event.preventDefault();
        showTab(tab.dataset.tab);
        // replaceState rather than pushState: switching tabs is not a place in history to go back
        // to, but a reload -- or the redirect after revoking a device -- should return here.
        history.replaceState(null, "", tab.href);
      });
    }
  }

  const output = document.getElementById("live-log-output");
  const status = document.getElementById("live-log-status");
  const clearButton = document.getElementById("clear-live-logs");
  if (!output || !status || !clearButton || typeof EventSource === "undefined") {
    return;
  }

  const maximumRows = 400;

  function setStatus(state, label) {
    status.className = "log-status " + state;
    const indicator = document.createElement("span");
    indicator.setAttribute("aria-hidden", "true");
    status.replaceChildren(indicator, document.createTextNode(" " + label));
  }

  function showEmpty(message) {
    const empty = document.createElement("p");
    empty.className = "log-empty";
    empty.textContent = message;
    output.replaceChildren(empty);
  }

  function appendLog(event) {
    const distanceFromBottom = output.scrollHeight - output.scrollTop - output.clientHeight;
    const shouldFollow = distanceFromBottom < 60;
    const empty = output.querySelector(".log-empty");
    if (empty) {
      empty.remove();
    }

    const row = document.createElement("div");
    row.className = "log-row";

    const timestamp = document.createElement("time");
    timestamp.className = "log-time";
    const parsedTime = new Date(event.time);
    timestamp.textContent = Number.isNaN(parsedTime.getTime()) ? String(event.time || "") : parsedTime.toLocaleString();

    const level = document.createElement("span");
    const normalizedLevel = String(event.level || "info").toLowerCase();
    level.className = "log-level " + normalizedLevel;
    level.textContent = normalizedLevel;

    const message = document.createElement("span");
    message.className = "log-message";
    message.textContent = String(event.message || "");

    row.append(timestamp, level, message);

    const attributes = Object.entries(event.attributes || {})
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, value]) => key + "=" + String(value))
      .join("  ");
    if (attributes) {
      const details = document.createElement("span");
      details.className = "log-attributes";
      details.textContent = attributes;
      row.append(details);
    }

    output.append(row);
    while (output.children.length > maximumRows) {
      output.firstElementChild.remove();
    }
    if (shouldFollow) {
      output.scrollTop = output.scrollHeight;
    }
  }

  const source = new EventSource("/admin/logs");
  source.addEventListener("ready", () => setStatus("live", "Live"));
  source.addEventListener("log", (message) => {
    try {
      appendLog(JSON.parse(message.data));
    } catch (_) {
      // Ignore a malformed event without interrupting later log delivery.
    }
  });
  source.onopen = () => setStatus("live", "Live");
  source.onerror = () => setStatus("reconnecting", "Reconnecting");

  clearButton.addEventListener("click", () => showEmpty("Waiting for new server events…"));
  window.addEventListener("pagehide", () => source.close(), { once: true });
})();
`

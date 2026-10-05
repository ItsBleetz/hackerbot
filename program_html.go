package main

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

type programHTMLData struct {
	Name              string
	Initial           string
	Handle            string
	ProgramURL        string
	PictureURL        string
	EventLabel        string
	EventClass        string
	CapturedAt        string
	Availability      string
	Rewards           string
	SubmissionState   string
	ProgramState      string
	StartedAccepting  string
	ScopeSummary      string
	ExclusionCount    int
	ResearcherSummary string
	Features          string
	Policy            string
	PolicyHTML        template.HTML
	PolicyLength      string
	Changes           []programHTMLChange
	Scopes            []programHTMLScope
	Exclusions        []programHTMLExclusion
	RawJSON           string
}

type programHTMLChange struct {
	Path   string
	Before string
	After  string
}

type programHTMLScope struct {
	ID                    string
	AssetType             string
	AssetIdentifier       string
	EligibleForSubmission string
	EligibleForBounty     string
	MaxSeverity           string
	SeverityClass         string
	Reference             string
	Confidentiality       string
	Integrity             string
	Availability          string
	Instruction           string
	CreatedAt             string
	UpdatedAt             string
}

type programHTMLExclusion struct {
	ID        string
	Category  string
	Details   string
	CreatedAt string
	UpdatedAt string
}

func programHTML(change ProgramChange) ([]byte, error) {
	snapshot := change.After
	if snapshot == nil {
		snapshot = change.Before
	}
	if snapshot == nil {
		return nil, fmt.Errorf("program HTML has no snapshot")
	}
	attrs := objectAttributes(snapshot.Program)
	name := stringValue(attrs["name"])
	if name == "" {
		name = snapshot.Handle
	}
	raw, err := programJSON(*snapshot)
	if err != nil {
		return nil, err
	}

	policy := stringValue(attrs["policy"])
	policyHTML, err := renderPolicyMarkdown(policy)
	if err != nil {
		return nil, err
	}
	data := programHTMLData{
		Name:              name,
		Initial:           firstRune(name),
		Handle:            snapshot.Handle,
		ProgramURL:        "https://hackerone.com/" + url.PathEscape(snapshot.Handle),
		PictureURL:        absoluteH1URL(stringValue(attrs["profile_picture"])),
		EventLabel:        htmlEventLabel(change.Kind),
		EventClass:        htmlEventClass(change.Kind),
		CapturedAt:        htmlTime(snapshot.CapturedAt),
		Availability:      programAvailability(attrs),
		Rewards:           programRewards(attrs),
		SubmissionState:   displayLabel(stringValue(attrs["submission_state"])),
		ProgramState:      displayLabel(stringValue(attrs["state"])),
		StartedAccepting:  stringValue(attrs["started_accepting_at"]),
		ScopeSummary:      strings.ReplaceAll(programScopeSummary(snapshot.Scopes), "**", ""),
		ExclusionCount:    len(snapshot.ScopeExclusions),
		ResearcherSummary: researcherProgramSummary(attrs),
		Features:          programFeatures(attrs),
		Policy:            policy,
		PolicyHTML:        policyHTML,
		PolicyLength:      formatInteger(len([]rune(policy))),
		RawJSON:           string(raw),
	}
	for _, detail := range change.Details {
		data.Changes = append(data.Changes, programHTMLChange{
			Path: humanChangePath(detail.Path), Before: safeField(detail.Before), After: safeField(detail.After),
		})
	}
	for _, rawScope := range snapshot.Scopes {
		id, _ := resourceID(rawScope)
		scope := objectAttributes(rawScope)
		data.Scopes = append(data.Scopes, programHTMLScope{
			ID:                    id,
			AssetType:             stringValueOrJSON(scope["asset_type"]),
			AssetIdentifier:       stringValueOrJSON(scope["asset_identifier"]),
			EligibleForSubmission: htmlBool(scope["eligible_for_submission"]),
			EligibleForBounty:     htmlBool(scope["eligible_for_bounty"]),
			MaxSeverity:           displayLabel(stringValueOrJSON(scope["max_severity"])),
			SeverityClass:         severityClass(stringValueOrJSON(scope["max_severity"])),
			Reference:             stringValueOrJSON(scope["reference"]),
			Confidentiality:       stringValueOrJSON(scope["confidentiality_requirement"]),
			Integrity:             stringValueOrJSON(scope["integrity_requirement"]),
			Availability:          stringValueOrJSON(scope["availability_requirement"]),
			Instruction:           stringValueOrJSON(scope["instruction"]),
			CreatedAt:             stringValueOrJSON(scope["created_at"]),
			UpdatedAt:             stringValueOrJSON(scope["updated_at"]),
		})
	}
	sort.SliceStable(data.Scopes, func(i, j int) bool {
		left, right := data.Scopes[i], data.Scopes[j]
		if severityRank(left.MaxSeverity) != severityRank(right.MaxSeverity) {
			return severityRank(left.MaxSeverity) < severityRank(right.MaxSeverity)
		}
		if strings.ToLower(left.AssetIdentifier) != strings.ToLower(right.AssetIdentifier) {
			return strings.ToLower(left.AssetIdentifier) < strings.ToLower(right.AssetIdentifier)
		}
		return left.ID < right.ID
	})
	for _, rawExclusion := range snapshot.ScopeExclusions {
		id, _ := resourceID(rawExclusion)
		exclusion := objectAttributes(rawExclusion)
		data.Exclusions = append(data.Exclusions, programHTMLExclusion{
			ID:        id,
			Category:  stringValueOrJSON(exclusion["category"]),
			Details:   stringValueOrJSON(exclusion["details"]),
			CreatedAt: stringValueOrJSON(exclusion["created_at"]),
			UpdatedAt: stringValueOrJSON(exclusion["updated_at"]),
		})
	}

	var output bytes.Buffer
	if err := programHTMLTemplate.Execute(&output, data); err != nil {
		return nil, fmt.Errorf("render program HTML: %w", err)
	}
	return output.Bytes(), nil
}

var policyMarkdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

func renderPolicyMarkdown(policy string) (template.HTML, error) {
	var output bytes.Buffer
	if err := policyMarkdown.Convert([]byte(policy), &output); err != nil {
		return "", fmt.Errorf("render program policy markdown: %w", err)
	}
	// Goldmark's safe renderer omits raw HTML and unsafe link destinations.
	// Marking only its generated output as HTML preserves formatting without
	// allowing program-controlled script markup into the attachment.
	return template.HTML(output.String()), nil
}

func htmlEventLabel(kind string) string {
	switch kind {
	case "new":
		return "New program"
	case "changed":
		return "Program updated"
	case "removed":
		return "Program removed"
	case "manual":
		return "Requested snapshot"
	default:
		return "Program update"
	}
}

func htmlEventClass(kind string) string {
	if kind == "new" {
		return "event-new"
	}
	if kind == "removed" {
		return "event-removed"
	}
	return "event-update"
}

func htmlBool(value any) string {
	if result, ok := boolAttribute(value); ok {
		if result {
			return "Yes"
		}
		return "No"
	}
	return "—"
}

func severityRank(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	case "none":
		return 4
	default:
		return 5
	}
}

func severityClass(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "critical", "high", "medium", "low", "none":
		return "severity-" + value
	default:
		return "severity-unknown"
	}
}

func htmlTime(value time.Time) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC().Format("2 January 2006, 15:04:05 UTC")
}

func firstRune(value string) string {
	for _, r := range value {
		return string(r)
	}
	return "H"
}

var programHTMLTemplate = template.Must(template.New("program").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Name}} — Hackerbot program snapshot</title>
  <style>
    :root { --ink:#17191c; --muted:#66717c; --line:#dfe3e6; --paper:#fff; --bg:#f5f6f7; --green:#24c875; --green-dark:#087f4f; --blue:#1769e0; --amber:#a56100; --red:#c13b3b; }
    * { box-sizing:border-box; }
    html { scroll-behavior:smooth; }
    body { margin:0; background:var(--bg); color:var(--ink); font:14px/1.55 Inter, ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    a { color:var(--blue); }
    .global-bar { height:58px; background:#121416; color:#fff; }
    .global-inner, .program-inner, .tabs-inner, main { width:min(1180px, calc(100% - 32px)); margin:auto; }
    .global-inner { height:100%; display:flex; align-items:center; justify-content:space-between; }
    .h1-brand { display:flex; align-items:center; gap:11px; color:#fff; text-decoration:none; font-weight:800; font-size:16px; }
    .h-mark { width:30px; height:30px; border:2px solid #fff; border-radius:50%; display:grid; place-items:center; font-size:14px; }
    .snapshot-label { color:#abb3b9; font-size:12px; font-weight:600; letter-spacing:.25px; }
    .program-header { background:#fff; border-bottom:1px solid var(--line); }
    .program-inner { min-height:178px; display:flex; gap:24px; align-items:center; padding:30px 0; }
    .avatar { width:104px; height:104px; flex:0 0 auto; border-radius:6px; object-fit:contain; border:1px solid var(--line); background:#fff; padding:5px; }
    .avatar-fallback { display:grid; place-items:center; padding:0; background:#edf8f2; color:var(--green-dark); font-size:38px; font-weight:800; }
    .hero-copy { flex:1; min-width:0; }
    .event { display:inline-flex; padding:3px 9px; border-radius:3px; font-size:11px; line-height:1.5; font-weight:800; text-transform:uppercase; letter-spacing:.55px; }
    .event-new { background:#dff8eb; color:#087f4f; } .event-update { background:#fff2d7; color:#8a5600; } .event-removed { background:#fde4e4; color:#9c2929; }
    h1 { margin:9px 0 3px; font-size:30px; line-height:1.18; overflow-wrap:anywhere; }
    .handle, .captured { color:var(--muted); } .handle { font-size:15px; } .captured { margin-top:8px; font-size:12px; }
    .open-button { color:#151719; background:#fff; border:1px solid #aeb5bb; border-radius:4px; padding:10px 15px; text-decoration:none; font-weight:750; white-space:nowrap; }
    .open-button:hover { border-color:#151719; }
    .tabs { background:#fff; border-bottom:1px solid var(--line); position:sticky; top:0; z-index:5; }
    .tabs-inner { display:flex; gap:26px; overflow:auto; }
    .tabs a { padding:16px 1px 13px; border-bottom:3px solid transparent; color:#4d565e; text-decoration:none; white-space:nowrap; font-weight:700; }
    .tabs a:first-child, .tabs a:hover { color:#151719; border-color:var(--green); }
    main { padding:26px 0 44px; }
    .layout { display:grid; grid-template-columns:minmax(0, 1fr) 300px; gap:22px; align-items:start; }
    .card { scroll-margin-top:72px; background:var(--paper); border:1px solid var(--line); border-radius:5px; margin:0 0 18px; box-shadow:0 1px 2px rgba(20,25,30,.035); }
    .card-head { padding:20px 22px 16px; border-bottom:1px solid #edf0f2; }
    .card-body { padding:22px; }
    h2 { font-size:19px; line-height:1.3; margin:0; } h3 { font-size:14px; margin:0 0 8px; }
    .section-meta { color:var(--muted); font-size:13px; margin-top:4px; }
    .notice { display:flex; gap:11px; padding:13px 15px; border-radius:4px; background:#eef7ff; border:1px solid #cfe5f7; color:#29475d; margin-bottom:18px; }
    .notice strong { color:#17384f; }
    .overview-grid { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:0; }
    .overview-item { padding:16px 18px; border-right:1px solid var(--line); border-bottom:1px solid var(--line); min-height:88px; }
    .overview-item:nth-child(2n) { border-right:0; }
    .overview-label, .side-label { color:var(--muted); font-size:11px; font-weight:800; text-transform:uppercase; letter-spacing:.55px; margin-bottom:5px; }
    .overview-value, .side-value { font-weight:700; white-space:pre-line; overflow-wrap:anywhere; }
    .policy { overflow-wrap:anywhere; font-family:inherit; font-size:14px; }
    .policy > :first-child { margin-top:0; } .policy > :last-child { margin-bottom:0; }
    .policy h1, .policy h2, .policy h3, .policy h4 { margin:1.35em 0 .55em; line-height:1.3; }
    .policy h1 { font-size:23px; } .policy h2 { font-size:19px; padding-bottom:7px; border-bottom:1px solid var(--line); } .policy h3 { font-size:16px; }
    .policy p, .policy ul, .policy ol { margin:.7em 0; } .policy li + li { margin-top:.3em; }
    .policy blockquote { margin:1em 0; padding:2px 15px; border-left:3px solid var(--green); color:#4d5962; background:#f8faf9; }
    .policy code { background:#f0f2f3; border-radius:3px; padding:2px 4px; font-family:ui-monospace, SFMono-Regular, Consolas, monospace; }
    .policy pre { overflow:auto; background:#171b1f; color:#e7ecef; padding:13px; border-radius:4px; }
    .policy table { min-width:600px; } .policy img { max-width:100%; }
    .policy-source { margin-top:20px; }
    .policy-source-text { max-height:420px; overflow:auto; white-space:pre-wrap; overflow-wrap:anywhere; background:#f7f8f9; padding:13px; border-radius:3px; font-family:ui-monospace, SFMono-Regular, Consolas, monospace; }
    .change { border:1px solid var(--line); border-radius:4px; margin:0 0 12px; overflow:hidden; }
    .change-path { background:#f3f7f5; color:#175c46; padding:9px 13px; font-weight:800; overflow-wrap:anywhere; }
    .change-grid { display:grid; grid-template-columns:1fr 1fr; }
    .change-side { min-width:0; padding:13px; } .change-side + .change-side { border-left:1px solid var(--line); }
    .before h3 { color:var(--red); } .after h3 { color:var(--green-dark); }
    .change-value, .raw { white-space:pre-wrap; overflow-wrap:anywhere; font-family:ui-monospace, SFMono-Regular, Consolas, monospace; }
    .change-value { margin:0; background:#f8f9fa; padding:11px; border-radius:3px; max-height:420px; overflow:auto; }
    .table-wrap { overflow:auto; border:1px solid var(--line); border-radius:4px; }
    table { width:100%; border-collapse:collapse; min-width:1360px; }
    th { position:sticky; top:0; background:#f3f5f6; color:#4c5760; text-align:left; font-size:11px; text-transform:uppercase; letter-spacing:.35px; }
    th, td { padding:11px 12px; border-bottom:1px solid var(--line); vertical-align:top; }
    tr:last-child td { border-bottom:0; } tbody tr:hover { background:#fafcfb; }
    .scope-table { min-width:780px; table-layout:auto; }
    .scope-type-col { width:105px; } .scope-asset-col { width:auto; } .scope-eligible-col { width:130px; } .scope-severity-col { width:112px; } .scope-updated-col { width:145px; }
    .scope-table th, .scope-table td { padding:14px 13px; }
    .asset-cell { min-width:310px; }
    .asset { display:block; white-space:nowrap; font:700 13px/1.4 ui-monospace, SFMono-Regular, Consolas, monospace; color:#1b2228; }
    .asset-meta { display:flex; flex-wrap:wrap; gap:5px 12px; margin-top:7px; color:var(--muted); font-size:11px; }
    .scope-instruction { margin-top:9px; color:#4e5962; font-size:12px; line-height:1.45; white-space:pre-wrap; overflow-wrap:anywhere; }
    .asset-type { display:inline-flex; min-width:82px; min-height:30px; align-items:center; justify-content:center; border-radius:4px; padding:5px 9px; background:#eef3f8; color:#315270; font-size:11px; font-weight:800; }
    .eligibility { white-space:nowrap; font-size:12px; line-height:1.8; }
    .eligibility b { display:inline-block; width:12px; }
    .severity { display:inline-flex; min-width:82px; justify-content:center; border-radius:4px; padding:5px 9px; font-size:11px; font-weight:850; text-transform:uppercase; letter-spacing:.25px; }
    .severity-critical { background:#f9d9dd; color:#9e2636; } .severity-high { background:#fde5d2; color:#9b4612; } .severity-medium { background:#fff0c7; color:#825f00; } .severity-low { background:#dcebf9; color:#285f91; } .severity-none, .severity-unknown { background:#e9ecef; color:#5d666e; }
    .updated { white-space:nowrap; color:#59646d; font-size:12px; }
    .pill { display:inline-block; border-radius:3px; padding:2px 7px; background:#eaf1fb; color:#315a9e; font-size:11px; font-weight:800; }
    .exclusion { border-left:3px solid #d89220; background:#fffbf0; padding:14px 16px; margin:0 0 11px; border-radius:3px; }
    .exclusion p { white-space:pre-wrap; overflow-wrap:anywhere; margin:5px 0; }
    .sidebar { position:sticky; top:72px; }
    .side-row { padding:14px 17px; border-bottom:1px solid var(--line); }
    .side-row:last-child { border-bottom:0; }
    .dot { display:inline-block; width:8px; height:8px; margin-right:6px; border-radius:50%; background:var(--green); }
    details { border:1px solid var(--line); border-radius:4px; padding:12px 14px; }
    summary { cursor:pointer; font-weight:800; }
    .raw { max-height:650px; overflow:auto; background:#15191d; color:#e3e9ed; padding:16px; border-radius:3px; }
    .empty { color:var(--muted); font-style:italic; padding:8px 0; }
    footer { color:var(--muted); text-align:center; padding-top:6px; font-size:12px; }
    @media (max-width:900px) { .layout { grid-template-columns:1fr; } .sidebar { position:static; } }
    @media (max-width:680px) { .global-inner, .program-inner, .tabs-inner, main { width:min(100% - 20px,1180px); } .program-inner { min-height:0; align-items:flex-start; padding:22px 0; flex-wrap:wrap; } .avatar { width:72px; height:72px; } .open-button { width:100%; text-align:center; } h1 { font-size:24px; } .overview-grid, .change-grid { grid-template-columns:1fr; } .overview-item { border-right:0; } .change-side + .change-side { border-left:0; border-top:1px solid var(--line); } .card-head, .card-body { padding:17px; } }
  </style>
</head>
<body>
  <header class="global-bar"><div class="global-inner"><a class="h1-brand" href="{{.ProgramURL}}"><span class="h-mark">H1</span><span>HackerOne</span></a><span class="snapshot-label">Read-only Hacker API snapshot</span></div></header>
  <div class="program-header"><div class="program-inner">
    {{if .PictureURL}}<img class="avatar" src="{{.PictureURL}}" alt="{{.Name}} program picture">{{else}}<div class="avatar avatar-fallback">{{.Initial}}</div>{{end}}
    <div class="hero-copy"><span class="event {{.EventClass}}">{{.EventLabel}}</span><h1>{{.Name}}</h1><div class="handle">@{{.Handle}}</div><div class="captured">Snapshot captured {{.CapturedAt}}</div></div>
    <a class="open-button" href="{{.ProgramURL}}">View program on HackerOne ↗</a>
  </div></div>
  <nav class="tabs"><div class="tabs-inner"><a href="#overview">Overview</a>{{if .Changes}}<a href="#changes">Changes</a>{{end}}<a href="#guidelines">Program guidelines</a><a href="#scope">Scope</a><a href="#exclusions">Exclusions</a><a href="#raw">API data</a></div></nav>
  <main>
    <div class="layout"><div class="main-column">
      <section class="card" id="overview"><div class="card-head"><h2>Program overview</h2><div class="section-meta">Information available to your researcher account.</div></div><div class="overview-grid">
        <div class="overview-item"><div class="overview-label">Availability</div><div class="overview-value">{{.Availability}}</div></div>
        <div class="overview-item"><div class="overview-label">Rewards</div><div class="overview-value">{{.Rewards}}</div></div>
        <div class="overview-item"><div class="overview-label">Structured scope</div><div class="overview-value">{{.ScopeSummary}}</div></div>
        <div class="overview-item"><div class="overview-label">Scope exclusions</div><div class="overview-value">{{.ExclusionCount}} documented</div></div>
        {{if .ResearcherSummary}}<div class="overview-item"><div class="overview-label">Your program history</div><div class="overview-value">{{.ResearcherSummary}}</div></div>{{end}}
        {{if .Features}}<div class="overview-item"><div class="overview-label">Program features</div><div class="overview-value">{{.Features}}</div></div>{{end}}
      </div></section>
      {{if .Changes}}<section class="card" id="changes"><div class="card-head"><h2>What changed</h2><div class="section-meta">{{len .Changes}} ordered change(s), with complete before and after values.</div></div><div class="card-body">{{range .Changes}}<article class="change"><div class="change-path">{{.Path}}</div><div class="change-grid"><div class="change-side before"><h3>Before</h3><pre class="change-value">{{.Before}}</pre></div><div class="change-side after"><h3>After</h3><pre class="change-value">{{.After}}</pre></div></div></article>{{end}}</div></section>{{end}}
      <section class="card" id="guidelines"><div class="card-head"><h2>Program guidelines</h2><div class="section-meta">Complete policy returned by HackerOne · {{.PolicyLength}} characters</div></div><div class="card-body">{{if .Policy}}<div class="policy">{{.PolicyHTML}}</div><details class="policy-source"><summary>Show exact policy source</summary><pre class="policy-source-text">{{.Policy}}</pre></details>{{else}}<div class="empty">No policy was returned by the Hacker API.</div>{{end}}</div></section>
      <section class="card" id="scope"><div class="card-head"><h2>Scope</h2><div class="section-meta">{{len .Scopes}} structured asset(s) · Critical, High, Medium, Low, then None</div></div><div class="card-body">{{if .Scopes}}<div class="table-wrap"><table class="scope-table"><colgroup><col class="scope-type-col"><col class="scope-asset-col"><col class="scope-eligible-col"><col class="scope-severity-col"><col class="scope-updated-col"></colgroup><thead><tr><th>Asset type</th><th>Asset identifier</th><th>Eligible</th><th>Max severity</th><th>Last updated</th></tr></thead><tbody>{{range .Scopes}}<tr><td><span class="asset-type">{{.AssetType}}</span></td><td class="asset-cell"><span class="asset">{{.AssetIdentifier}}</span><div class="asset-meta"><span>ID {{.ID}}</span>{{if .Reference}}<span>Reference {{.Reference}}</span>{{end}}{{if .Confidentiality}}<span>C: {{.Confidentiality}}</span>{{end}}{{if .Integrity}}<span>I: {{.Integrity}}</span>{{end}}{{if .Availability}}<span>A: {{.Availability}}</span>{{end}}{{if .CreatedAt}}<span>Created {{.CreatedAt}}</span>{{end}}</div>{{if .Instruction}}<div class="scope-instruction">{{.Instruction}}</div>{{end}}</td><td><div class="eligibility"><div><b>{{if eq .EligibleForBounty "Yes"}}✓{{else}}—{{end}}</b> Bounty</div><div><b>{{if eq .EligibleForSubmission "Yes"}}✓{{else}}—{{end}}</b> Submission</div></div></td><td><span class="severity {{.SeverityClass}}">{{.MaxSeverity}}</span></td><td class="updated">{{if .UpdatedAt}}{{.UpdatedAt}}{{else}}—{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<div class="empty">No structured scope was returned by the Hacker API.</div>{{end}}</div></section>
      <section class="card" id="exclusions"><div class="card-head"><h2>Scope exclusions</h2><div class="section-meta">Additional categories excluded from rewards.</div></div><div class="card-body">{{if .Exclusions}}{{range .Exclusions}}<article class="exclusion"><h3>{{if .Category}}{{.Category}}{{else}}Exclusion {{.ID}}{{end}}</h3><p>{{.Details}}</p><small>ID {{.ID}}{{if .UpdatedAt}} · Updated {{.UpdatedAt}}{{end}}</small></article>{{end}}{{else}}<div class="empty">No program-specific scope exclusions were returned.</div>{{end}}</div></section>
      <section class="card" id="raw"><div class="card-head"><h2>Complete API snapshot</h2><div class="section-meta">Exact program, structured scope, and exclusion JSON used for this page.</div></div><div class="card-body"><details><summary>Show raw JSON</summary><pre class="raw">{{.RawJSON}}</pre></details></div></section>
    </div><aside class="sidebar">
      <section class="card"><div class="card-head"><h2>Program at a glance</h2></div>
        <div class="side-row"><div class="side-label">Submissions</div><div class="side-value"><span class="dot"></span>{{.SubmissionState}}</div></div>
        <div class="side-row"><div class="side-label">Program visibility</div><div class="side-value">{{.ProgramState}}</div></div>
        <div class="side-row"><div class="side-label">Rewards</div><div class="side-value">{{.Rewards}}</div></div>
        <div class="side-row"><div class="side-label">Scope</div><div class="side-value">{{len .Scopes}} assets · {{.ExclusionCount}} exclusions</div></div>
        {{if .StartedAccepting}}<div class="side-row"><div class="side-label">Started accepting</div><div class="side-value">{{.StartedAccepting}}</div></div>{{end}}
      </section>
      <section class="card"><div class="card-head"><h2>Web-only metrics</h2></div><div class="side-row"><div class="side-value">Response efficiency, bounty averages, program statistics, top hackers, thanks, updates, and collaborators are not returned by the documented researcher API.</div></div></section>
    </aside></div>
    <footer>Generated by Hackerbot from read-only HackerOne Hacker API responses. This is a local snapshot, not a HackerOne-hosted page.</footer>
  </main>
</body>
</html>`))

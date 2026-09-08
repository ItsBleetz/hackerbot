package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type DiscordClient struct {
	http *http.Client
}

type discordPayload struct {
	Username        string                 `json:"username,omitempty"`
	Content         string                 `json:"content,omitempty"`
	Embeds          []discordEmbed         `json:"embeds,omitempty"`
	ThreadName      string                 `json:"thread_name,omitempty"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	URL         string              `json:"url,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Thumbnail   *discordThumbnail   `json:"thumbnail,omitempty"`
	Footer      *discordFooter      `json:"footer,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordThumbnail struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}

type discordFile struct {
	Name string
	Data []byte
}

func newDiscordClient(timeout time.Duration) *DiscordClient {
	return &DiscordClient{http: &http.Client{Timeout: timeout}}
}

func (d *DiscordClient) SendProgram(ctx context.Context, webhook string, change ProgramChange) error {
	snapshot := change.After
	if snapshot == nil {
		snapshot = change.Before
	}
	if snapshot == nil {
		return fmt.Errorf("program notification has no snapshot")
	}

	attrs := objectAttributes(snapshot.Program)
	name := stringValue(attrs["name"])
	if name == "" {
		name = snapshot.Handle
	}
	baseName := safeFilename(snapshot.Handle)
	htmlFileName := baseName + "-program-" + filenameTimestamp(snapshot.CapturedAt) + ".html"

	overview := programOverviewEmbed(name, attrs, *snapshot, change, htmlFileName)
	embeds := []discordEmbed{overview}
	if len(change.Details) > 0 {
		embeds = append(embeds, changePreviewEmbed(change.Details, htmlFileName, programColor(change.Kind)))
	}

	htmlFile, err := programHTML(change)
	if err != nil {
		return err
	}
	// Two awaited webhook calls guarantee that Discord accepts the summary
	// before Hackerbot begins uploading the complete HTML page.
	if err := d.send(ctx, webhook, discordPayload{
		Username:        "Hackerbot",
		Embeds:          embeds,
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}, nil); err != nil {
		return fmt.Errorf("send program summary: %w", err)
	}
	return d.send(ctx, webhook, discordPayload{
		Username:        "Hackerbot",
		Content:         "📎 Complete HTML program page for **" + name + "**: `" + htmlFileName + "`",
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}, []discordFile{{Name: htmlFileName, Data: htmlFile}})
}

func (d *DiscordClient) SendProgramDivider(ctx context.Context, webhook string) error {
	return d.send(ctx, webhook, discordPayload{
		Username:        "Hackerbot",
		Content:         "━━━━━━━━━━━━━━━━━━  ◆  ━━━━━━━━━━━━━━━━━━",
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}, nil)
}

func filenameTimestamp(value time.Time) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC().Format("20060102T150405Z")
}

func (d *DiscordClient) SendReport(ctx context.Context, webhook string, change ReportChange, mode string, notifyOwnComments, threadsEnabled bool) (string, error) {
	snapshot := change.After
	if snapshot == nil {
		snapshot = change.Before
	}
	if snapshot == nil {
		return "", fmt.Errorf("report notification has no snapshot")
	}
	messages := reportNotificationMessages(change, mode, notifyOwnComments)
	if len(messages) == 0 {
		return snapshot.DiscordThreadID, nil
	}
	threadID := snapshot.DiscordThreadID
	for index, message := range messages {
		payload := discordPayload{
			Username:        "Hackerbot Reports",
			Content:         message,
			AllowedMentions: discordAllowedMentions{Parse: []string{}},
		}
		if threadsEnabled && threadID == "" && index == 0 {
			payload.ThreadName = reportThreadName(change, mode)
			response, err := d.sendResult(ctx, webhook, payload, nil, "")
			if err != nil {
				return "", fmt.Errorf("create Discord report thread (the webhook must belong to a Forum or Media channel): %w", err)
			}
			threadID, err = discordMessageChannelID(response)
			if err != nil {
				return "", err
			}
			continue
		}
		targetThread := ""
		if threadsEnabled {
			targetThread = threadID
		}
		if _, err := d.sendResult(ctx, webhook, payload, nil, targetThread); err != nil {
			return threadID, err
		}
	}
	return threadID, nil
}

func reportNotificationMessages(change ReportChange, mode string, notifyOwnComments bool) []string {
	id := plainToken(change.ID)
	title := reportTitle(change)
	if change.Kind == "new" {
		return []string{reportCardHeader("🆕", "New report", id, title, mode) + "\n**Event:** Created"}
	}
	if change.Kind == "preview" {
		return []string{reportCardHeader("🧪", "Live report test", id, title, mode) + "\n**Timeline:** Existing activities follow from oldest to newest"}
	}
	if change.Before == nil || change.After == nil {
		return nil
	}

	var messages []string
	beforeState := stringValue(objectAttributes(change.Before.Report)["state"])
	afterState := stringValue(objectAttributes(change.After.Report)["state"])
	if beforeState != afterState && afterState != "" {
		messages = append(messages, reportTransitionCard("🔄", "Status changed", id, title, mode, "Status", beforeState, afterState))
	}

	beforeSeverity := reportSeverityRating(change.Before.Report)
	afterSeverity := reportSeverityRating(change.After.Report)
	if beforeSeverity != afterSeverity && (beforeSeverity != "" || afterSeverity != "") {
		messages = append(messages, reportTransitionCard(severityIcon(afterSeverity), "Severity changed", id, title, mode, "Severity", beforeSeverity, afterSeverity))
	}

	newActivities := newReportActivities(change.Before.Report, change.After.Report)
	for _, activity := range newActivities {
		if isCommentActivity(activity) {
			if !notifyOwnComments && isOwnReportComment(change.After.Report, activity) {
				continue
			}
			messages = append(messages, reportCommentMessages(id, title, activity, mode)...)
			continue
		}
		// HackerOne normally records a state transition as an activity as well as
		// changing report.attributes.state. The state message above is clearer,
		// so avoid sending the same event twice.
		if beforeState != afterState && activityRepresentsStateChange(activity) {
			continue
		}
		if beforeSeverity != afterSeverity && activityRepresentsSeverityChange(activity) {
			continue
		}
		messages = append(messages, reportActivityMessages(id, title, activity, mode)...)
	}
	return messages
}

func reportCardHeader(icon, event, id, title, mode string) string {
	header := "### " + icon + " " + event + "\n**Report:** [#" + id + "](https://hackerone.com/reports/" + url.PathEscape(id) + ")"
	if mode == "detailed" {
		header += "\n**Title:** " + escapeDiscordMarkdown(title)
	}
	return header
}

func reportTransitionCard(icon, event, id, title, mode, field, before, after string) string {
	before = transitionLabel(before)
	after = transitionLabel(after)
	return reportCardHeader(icon, event, id, title, mode) + "\n**" + field + ":** `" + escapeDiscordMarkdown(before) + "` → `" + escapeDiscordMarkdown(after) + "`"
}

func transitionLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Not set"
	}
	return displayLabel(value)
}

func reportSeverityRating(report json.RawMessage) string {
	if rating := strings.ToLower(strings.TrimSpace(stringValue(objectAttributes(report)["severity_rating"]))); rating != "" {
		return rating
	}
	severity := relationshipResource(report, "severity")
	return strings.ToLower(strings.TrimSpace(stringValue(objectAttributes(severity)["rating"])))
}

func severityIcon(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return "🔴"
	case "high":
		return "🟠"
	case "medium":
		return "🟡"
	case "low":
		return "🟢"
	default:
		return "⚪"
	}
}

func reportSummaryMessages(change ReportChange) []string {
	return reportNotificationMessages(change, "summary", true)
}

func reportTitle(change ReportChange) string {
	if change.After != nil {
		if title := strings.TrimSpace(stringValue(objectAttributes(change.After.Report)["title"])); title != "" {
			return title
		}
	}
	if change.Before != nil {
		if title := strings.TrimSpace(stringValue(objectAttributes(change.Before.Report)["title"])); title != "" {
			return title
		}
	}
	return "Untitled report"
}

func isCommentActivity(activity json.RawMessage) bool {
	var resource struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(activity, &resource) == nil && strings.EqualFold(resource.Type, "activity-comment")
}

func newReportActivities(before, after json.RawMessage) []json.RawMessage {
	oldActivities := resourcesByID(relationshipResources(before, "activities"))
	var result []json.RawMessage
	for _, activity := range relationshipResources(after, "activities") {
		activityID, err := resourceID(activity)
		if err == nil {
			if _, exists := oldActivities[activityID]; exists {
				continue
			}
		}
		result = append(result, activity)
	}
	return result
}

func activityType(activity json.RawMessage) string {
	var resource struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(activity, &resource)
	return strings.ToLower(strings.TrimSpace(resource.Type))
}

func activityLabel(activity json.RawMessage) string {
	kind := activityType(activity)
	known := map[string]string{
		"activity-bounty-awarded":        "Bounty awarded",
		"activity-bounty-suggested":      "Bounty suggested",
		"activity-bug-duplicate":         "Marked as duplicate",
		"activity-bug-informative":       "Marked as informative",
		"activity-bug-needs-more-info":   "More information requested",
		"activity-bug-new":               "Status changed to new",
		"activity-bug-not-applicable":    "Marked as not applicable",
		"activity-bug-reopened":          "Report reopened",
		"activity-bug-resolved":          "Report resolved",
		"activity-bug-triaged":           "Report triaged",
		"activity-comments-closed":       "Comments closed",
		"activity-comments-reopened":     "Comments reopened",
		"activity-cve-id-added":          "CVE ID added",
		"activity-group-assigned-to-bug": "Group assigned",
		"activity-reference-id-added":    "Reference ID added",
		"activity-user-assigned-to-bug":  "User assigned",
	}
	if label := known[kind]; label != "" {
		return label
	}
	kind = strings.TrimPrefix(kind, "activity-")
	kind = strings.TrimPrefix(kind, "bug-")
	kind = strings.NewReplacer("-", " ", "_", " ").Replace(kind)
	return displayLabel(kind)
}

func activityRepresentsStateChange(activity json.RawMessage) bool {
	kind := activityType(activity)
	if strings.Contains(kind, "state-changed") || strings.Contains(kind, "status-changed") {
		return true
	}
	for _, suffix := range []string{
		"bug-duplicate", "bug-informative", "bug-needs-more-info", "bug-new",
		"bug-not-applicable", "bug-reopened", "bug-resolved", "bug-triaged",
	} {
		if strings.HasSuffix(kind, suffix) {
			return true
		}
	}
	return false
}

func activityRepresentsSeverityChange(activity json.RawMessage) bool {
	kind := activityType(activity)
	return strings.Contains(kind, "severity") && (strings.Contains(kind, "changed") || strings.Contains(kind, "updated"))
}

func reportActivityMessages(id, title string, activity json.RawMessage, mode string) []string {
	header := reportCardHeader("⚡", activityLabel(activity), id, title, mode)
	if actor := activityActorName(activity); actor != "" {
		header += "\n**Actor:** " + escapeDiscordMarkdown(actor)
	}
	details := activityAttributeDetails(activity, mode)
	if details == "" {
		return []string{header}
	}
	return splitDiscordText(header+"\n\n"+details, "↳ **Activity continued — report `#"+id+"`**\n\n")
}

func activityActorName(activity json.RawMessage) string {
	actor := relationshipResource(activity, "actor")
	attrs := objectAttributes(actor)
	for _, key := range []string{"username", "name"} {
		if value := strings.TrimSpace(stringValue(attrs[key])); value != "" {
			return value
		}
	}
	return ""
}

func activityAttributeDetails(activity json.RawMessage, mode string) string {
	attrs := objectAttributes(activity)
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		switch strings.ToLower(key) {
		case "created_at", "updated_at":
			continue
		}
		if mode == "summary" && strings.EqualFold(key, "title") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(compactValue(attrs[key]))
		if value == "" || value == "null" || value == "{}" || value == "[]" {
			continue
		}
		lines = append(lines, "• **"+escapeDiscordMarkdown(displayLabel(key))+":** "+escapeDiscordMarkdown(abbreviate(value, 700)))
	}
	return strings.Join(lines, "\n")
}

func isOwnReportComment(report, activity json.RawMessage) bool {
	reporter := relationshipResource(report, "reporter")
	actor := relationshipResource(activity, "actor")
	if len(reporter) == 0 || len(actor) == 0 {
		return false
	}
	reporterID, reporterErr := resourceID(reporter)
	actorID, actorErr := resourceID(actor)
	if reporterErr == nil && actorErr == nil && reporterID != "" && actorID != "" {
		return reporterID == actorID
	}
	reporterUsername := stringValue(objectAttributes(reporter)["username"])
	actorUsername := stringValue(objectAttributes(actor)["username"])
	return reporterUsername != "" && strings.EqualFold(reporterUsername, actorUsername)
}

func relationshipResource(raw json.RawMessage, name string) json.RawMessage {
	var root struct {
		Relationships map[string]struct {
			Data json.RawMessage `json:"data"`
		} `json:"relationships"`
	}
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	return root.Relationships[name].Data
}

func reportCommentMessages(id, title string, activity json.RawMessage, mode string) []string {
	header := reportCardHeader("💬", "New comment", id, title, mode)
	if actor := activityActorName(activity); actor != "" {
		header += "\n**Author:** " + escapeDiscordMarkdown(actor)
	}
	if mode == "summary" {
		return []string{header + "\n**Content:** _Hidden in summary mode_"}
	}
	body := stringValue(objectAttributes(activity)["message"])
	if strings.TrimSpace(body) == "" {
		return []string{header + "\n\n**Comment**\n_The Hacker API returned an empty comment body._"}
	}
	return splitDiscordText(header+"\n\n**Comment**\n"+body, "↳ **Comment continued — report `#"+id+"`**\n\n")
}

func splitDiscordText(body, continuationPrefix string) []string {
	const limit = 2000
	remaining := []rune(body)
	prefix := ""
	var messages []string
	for len(remaining) > 0 {
		available := limit - len([]rune(prefix))
		if available < 1 {
			available = 1
		}
		take := min(available, len(remaining))
		messages = append(messages, prefix+string(remaining[:take]))
		remaining = remaining[take:]
		prefix = continuationPrefix
	}
	return messages
}

func escapeDiscordMarkdown(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	return strings.NewReplacer("*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>").Replace(value)
}

func reportThreadName(change ReportChange, mode string) string {
	prefix := "Report #"
	if change.Kind == "preview" {
		prefix = "TEST • Report #"
	}
	name := prefix + plainToken(change.ID)
	if mode == "detailed" {
		name += " • " + reportTitle(change)
	}
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, name)
	return abbreviate(strings.TrimSpace(name), 100)
}

func discordMessageChannelID(body []byte) (string, error) {
	var message struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal(body, &message); err != nil || message.ChannelID == "" {
		return "", fmt.Errorf("Discord did not return the created report thread ID")
	}
	return message.ChannelID, nil
}

func plainToken(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, value)
	return abbreviate(strings.TrimSpace(value), 200)
}

func programOverviewEmbed(name string, attrs map[string]any, snapshot ProgramSnapshot, change ProgramChange, htmlFileName string) discordEmbed {
	kindLabel := map[string]string{
		"new":     "🚨 New Program!",
		"changed": "📝 Program Changed",
		"removed": "⛔ Program Removed",
		"manual":  "🔎 Program Snapshot",
	}[change.Kind]
	if kindLabel == "" {
		kindLabel = "📣 Program Update"
	}
	description := map[string]string{
		"new":     "A new program is available to your HackerOne researcher account.",
		"changed": fmt.Sprintf("Hackerbot detected **%d ordered change(s)** since the previous snapshot.", len(change.Details)),
		"manual":  "Current program information requested from the HackerOne Hacker API.",
		"removed": "This program is no longer returned for your researcher account.",
	}[change.Kind]
	if description == "" {
		description = "Program information received from the HackerOne Hacker API."
	}

	embed := discordEmbed{
		Title:       abbreviate(kindLabel+" • "+name, 256),
		URL:         "https://hackerone.com/" + url.PathEscape(snapshot.Handle),
		Description: description,
		Color:       programColor(change.Kind),
		Timestamp:   notificationTimestamp(snapshot.CapturedAt),
		Footer:      &discordFooter{Text: "Hackerbot • Complete HTML program report attached"},
		Fields: []discordEmbedField{
			{Name: "🏷️ Handle", Value: "`" + abbreviate(snapshot.Handle, 200) + "`", Inline: true},
			{Name: "🚦 Availability", Value: programAvailability(attrs), Inline: true},
			{Name: "🎁 Rewards", Value: programRewards(attrs), Inline: true},
			{Name: "🎯 Scope", Value: programScopeSummary(snapshot.Scopes), Inline: true},
			{Name: "🚫 Exclusions", Value: fmt.Sprintf("**%d** documented", len(snapshot.ScopeExclusions)), Inline: true},
		},
	}
	if picture := absoluteH1URL(stringValue(attrs["profile_picture"])); picture != "" {
		embed.Thumbnail = &discordThumbnail{URL: picture}
	}
	if history := researcherProgramSummary(attrs); history != "" {
		embed.Fields = append(embed.Fields, discordEmbedField{Name: "👤 Your history", Value: history, Inline: true})
	}
	if features := programFeatures(attrs); features != "" {
		embed.Fields = append(embed.Fields, discordEmbedField{Name: "⚡ Features", Value: features, Inline: false})
	}
	if policy := stringValue(attrs["policy"]); policy != "" {
		embed.Fields = append(embed.Fields, discordEmbedField{
			Name:   "📜 Program policy",
			Value:  fmt.Sprintf("Complete **%s-character** policy is inside `%s`.", formatInteger(len([]rune(policy))), htmlFileName),
			Inline: false,
		})
	}
	return embed
}

func changePreviewEmbed(details []ChangeDetail, attachment string, color int) discordEmbed {
	embed := discordEmbed{
		Title:       "🔄 What changed",
		Description: fmt.Sprintf("Changed parts are ordered by API field and resource ID. Complete, uncut values are inside `%s`.", attachment),
		Color:       color,
	}
	appendChangePreview(&embed, details, attachment)
	shown := min(len(details), 4)
	footer := fmt.Sprintf("Showing %d of %d change(s)", shown, len(details))
	if shown < len(details) {
		footer += " • Every change is in the HTML attachment"
	}
	embed.Footer = &discordFooter{Text: footer}
	return embed
}

func appendChangePreview(embed *discordEmbed, details []ChangeDetail, attachment string) {
	remaining := min(4, 25-len(embed.Fields))
	for index := 0; index < len(details) && index < remaining; index++ {
		detail := details[index]
		embed.Fields = append(embed.Fields, discordEmbedField{
			Name:   abbreviate(humanChangePath(detail.Path), 256),
			Value:  changeFieldValue(detail, attachment),
			Inline: false,
		})
	}
}

func changeFieldValue(detail ChangeDetail, attachment string) string {
	before := changeValuePreview(safeField(detail.Before), attachment, 240)
	after := changeValuePreview(safeField(detail.After), attachment, 240)
	return "**Before**\n" + before + "\n**After**\n" + after
}

func changeValuePreview(value, attachment string, max int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) <= max {
		return value
	}
	marker := "\n↳ _Full value in `" + attachment + "`_"
	available := max - len([]rune(marker)) - 1
	if available < 1 {
		return marker
	}
	return string([]rune(value)[:available]) + "…" + marker
}

func humanChangePath(path string) string {
	path = strings.TrimPrefix(path, "program.attributes.")
	path = strings.TrimPrefix(path, "program.")
	path = strings.ReplaceAll(path, "scope_exclusion", "Scope exclusion")
	path = strings.ReplaceAll(path, "scope", "Scope")
	path = strings.ReplaceAll(path, ".attributes.", " • ")
	path = strings.ReplaceAll(path, ".relationships.", " • ")
	path = strings.ReplaceAll(path, ".", " • ")
	path = strings.ReplaceAll(path, "_", " ")
	if path == "" {
		return "Program data"
	}
	runes := []rune(path)
	if runes[0] >= 'a' && runes[0] <= 'z' {
		runes[0] -= 'a' - 'A'
	}
	return string(runes)
}

func programAvailability(attrs map[string]any) string {
	submission := strings.ToLower(stringValue(attrs["submission_state"]))
	prefix := "⚪"
	if submission == "open" {
		prefix = "🟢"
	} else if submission == "closed" || submission == "paused" {
		prefix = "🔴"
	}
	value := prefix + " " + displayLabel(submission)
	if state := stringValue(attrs["state"]); state != "" {
		value += "\n" + displayLabel(state)
	}
	return value
}

func programRewards(attrs map[string]any) string {
	if offered, ok := boolAttribute(attrs["offers_bounties"]); ok && offered {
		currency := strings.ToUpper(stringValue(attrs["currency"]))
		if currency != "" {
			return "💰 Bounties • " + currency
		}
		return "💰 Bounties offered"
	}
	return "🏅 Recognition"
}

func programScopeSummary(scopes []json.RawMessage) string {
	bountyEligible := 0
	submissionEligible := 0
	severityCounts := make(map[string]int)
	for _, scope := range scopes {
		attrs := objectAttributes(scope)
		if value, ok := boolAttribute(attrs["eligible_for_bounty"]); ok && value {
			bountyEligible++
		}
		if value, ok := boolAttribute(attrs["eligible_for_submission"]); ok && value {
			submissionEligible++
		}
		if severity := strings.ToLower(stringValue(attrs["max_severity"])); severity != "" {
			severityCounts[severity]++
		}
	}
	value := fmt.Sprintf("**%d** assets\n%d bounty • %d submission", len(scopes), bountyEligible, submissionEligible)
	var severityParts []string
	for _, severity := range []string{"critical", "high", "medium", "low", "none"} {
		if count := severityCounts[severity]; count > 0 {
			severityParts = append(severityParts, displayLabel(severity)+" "+strconv.Itoa(count))
		}
	}
	if len(severityParts) > 0 {
		value += "\n" + strings.Join(severityParts, " • ")
	}
	return abbreviate(value, 1024)
}

func researcherProgramSummary(attrs map[string]any) string {
	_, reportsPresent := attrs["number_of_reports_for_user"]
	_, validPresent := attrs["number_of_valid_reports_for_user"]
	_, bountyPresent := attrs["bounty_earned_for_user"]
	if !reportsPresent && !validPresent && !bountyPresent {
		return ""
	}
	value := fmt.Sprintf("%s reports • %s valid", compactValue(attrs["number_of_reports_for_user"]), compactValue(attrs["number_of_valid_reports_for_user"]))
	if bountyPresent {
		currency := strings.ToUpper(stringValue(attrs["currency"]))
		value += "\n" + compactValue(attrs["bounty_earned_for_user"])
		if currency != "" {
			value += " " + currency
		}
		value += " earned"
	}
	return abbreviate(value, 1024)
}

func programFeatures(attrs map[string]any) string {
	features := []struct {
		Key   string
		Label string
	}{
		{"triage_active", "Active triage"},
		{"fast_payments", "Fast payments"},
		{"gold_standard_safe_harbor", "Gold Standard Safe Harbor"},
		{"allows_bounty_splitting", "Bounty splitting"},
		{"open_scope", "Open scope"},
	}
	var parts []string
	for _, feature := range features {
		if enabled, ok := boolAttribute(attrs[feature.Key]); ok {
			icon := "➖"
			if enabled {
				icon = "✅"
			}
			parts = append(parts, icon+" "+feature.Label)
		}
	}
	return strings.Join(parts, "  •  ")
}

func boolAttribute(value any) (bool, bool) {
	result, ok := value.(bool)
	return result, ok
}

func displayLabel(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "_", " "))
	if value == "" {
		return "Unknown"
	}
	runes := []rune(value)
	if runes[0] >= 'a' && runes[0] <= 'z' {
		runes[0] -= 'a' - 'A'
	}
	return string(runes)
}

func formatInteger(value int) string {
	digits := strconv.Itoa(value)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

func notificationTimestamp(value time.Time) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC().Format(time.RFC3339)
}

func programColor(kind string) int {
	switch kind {
	case "new":
		return 0x57F287
	case "removed":
		return 0xED4245
	case "manual":
		return 0x5865F2
	default:
		return 0xFEE75C
	}
}

func (d *DiscordClient) send(ctx context.Context, webhook string, payload discordPayload, files []discordFile) error {
	_, err := d.sendResult(ctx, webhook, payload, files, "")
	return err
}

func (d *DiscordClient) sendResult(ctx context.Context, webhook string, payload discordPayload, files []discordFile, threadID string) ([]byte, error) {
	if strings.TrimSpace(webhook) == "" {
		return nil, fmt.Errorf("Discord webhook is not configured")
	}
	for attempt := 0; attempt < 5; attempt++ {
		body, contentType, err := multipartPayload(payload, files)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, withWaitAndThread(webhook, threadID), body)
		if err != nil {
			return nil, fmt.Errorf("create Discord request: %w", err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("User-Agent", "hackerbot/"+version)
		resp, err := d.http.Do(req)
		if err != nil {
			return nil, discordTransportError(err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read Discord response: %w", readErr)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return responseBody, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			delay := discordRetryDelay(responseBody, attempt)
			if err := waitContext(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("Discord webhook returned HTTP %d: %s", resp.StatusCode, abbreviate(string(responseBody), 500))
	}
	return nil, fmt.Errorf("Discord webhook retries exhausted")
}

func discordTransportError(err error) error {
	// net/http wraps transport failures in url.Error, whose Error method includes
	// the full request URL. Discord webhook URLs contain a secret token.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("send Discord webhook (%s): %w", urlErr.Op, urlErr.Err)
	}
	return fmt.Errorf("send Discord webhook: %w", err)
}

func multipartPayload(payload discordPayload, files []discordFile) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	payloadPart, err := writer.CreateFormField("payload_json")
	if err != nil {
		return nil, "", err
	}
	if err := json.NewEncoder(payloadPart).Encode(payload); err != nil {
		return nil, "", err
	}
	for index, file := range files {
		part, err := writer.CreateFormFile("files["+strconv.Itoa(index)+"]", file.Name)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(file.Data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &body, writer.FormDataContentType(), nil
}

func withWaitAndThread(webhook, threadID string) string {
	parsed, err := url.Parse(webhook)
	if err != nil {
		return webhook
	}
	query := parsed.Query()
	query.Set("wait", "true")
	if threadID != "" {
		query.Set("thread_id", threadID)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func discordRetryDelay(body []byte, attempt int) time.Duration {
	var response struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &response) == nil && response.RetryAfter > 0 {
		return time.Duration(response.RetryAfter * float64(time.Second))
	}
	return time.Duration(1<<attempt) * time.Second
}

func programJSON(snapshot ProgramSnapshot) ([]byte, error) {
	payload := struct {
		CapturedAt      time.Time         `json:"captured_at"`
		Program         json.RawMessage   `json:"program"`
		Scopes          []json.RawMessage `json:"structured_scopes"`
		ScopeExclusions []json.RawMessage `json:"scope_exclusions"`
	}{snapshot.CapturedAt, snapshot.Program, snapshot.Scopes, snapshot.ScopeExclusions}
	return json.MarshalIndent(payload, "", "  ")
}

func objectAttributes(raw json.RawMessage) map[string]any {
	var envelope struct {
		Attributes map[string]any `json:"attributes"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return envelope.Attributes
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func stringValueOrJSON(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	b, _ := json.Marshal(value)
	return string(b)
}

func safeField(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func absoluteH1URL(value string) string {
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	if strings.HasPrefix(value, "/") {
		return "https://hackerone.com" + value
	}
	return ""
}

func safeFilename(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "hackerone"
	}
	result := b.String()
	if len(result) > 80 {
		result = result[:80]
	}
	return result
}

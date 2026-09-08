package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"
)

type Monitor struct {
	cfg     Config
	h1      *H1Client
	discord *DiscordClient
	store   *StateStore
}

func newMonitor(cfg Config, store *StateStore) *Monitor {
	return &Monitor{
		cfg:     cfg,
		h1:      newH1Client(cfg),
		discord: newDiscordClient(cfg.timeout),
		store:   store,
	}
}

func (m *Monitor) FetchProgram(ctx context.Context, handle string) (ProgramSnapshot, error) {
	program, err := m.h1.Program(ctx, handle)
	if err != nil {
		return ProgramSnapshot{}, fmt.Errorf("fetch program %s: %w", handle, err)
	}
	scopes, err := m.h1.Scopes(ctx, handle)
	if err != nil {
		return ProgramSnapshot{}, fmt.Errorf("fetch scopes for %s: %w", handle, err)
	}
	exclusions, err := m.h1.ScopeExclusions(ctx, handle)
	if err != nil {
		return ProgramSnapshot{}, fmt.Errorf("fetch scope exclusions for %s: %w", handle, err)
	}
	sortResources(scopes)
	sortResources(exclusions)
	return ProgramSnapshot{
		Handle:          handle,
		Program:         program,
		Scopes:          scopes,
		ScopeExclusions: exclusions,
		CapturedAt:      time.Now().UTC(),
	}, nil
}

func (m *Monitor) SendHandle(ctx context.Context, handle string) error {
	snapshot, err := m.FetchProgram(ctx, handle)
	if err != nil {
		return err
	}
	return m.discord.SendProgram(ctx, m.cfg.ProgramWebhookURL, ProgramChange{
		Kind:   "manual",
		Handle: handle,
		After:  &snapshot,
	})
}

func (m *Monitor) CheckPrograms(ctx context.Context) error {
	listed, err := m.h1.Programs(ctx)
	if err != nil {
		return fmt.Errorf("list programs: %w", err)
	}
	handles := make([]string, 0, len(listed))
	for _, raw := range listed {
		handle, err := programHandle(raw)
		if err != nil {
			return fmt.Errorf("decode listed program: %w", err)
		}
		handles = append(handles, handle)
	}
	sort.Strings(handles)

	current := make(map[string]ProgramSnapshot, len(handles))
	for _, handle := range handles {
		snapshot, err := m.FetchProgram(ctx, handle)
		if err != nil {
			return err
		}
		current[handle] = snapshot
	}

	previous, err := m.store.Snapshot()
	if err != nil {
		return fmt.Errorf("read program state: %w", err)
	}
	if !previous.ProgramsInitialized {
		if err := m.store.InitializePrograms(current); err != nil {
			return err
		}
		log.Printf("program baseline created with %d programs; no Discord messages sent", len(current))
		return nil
	}
	sentProgramNotification := false
	sendProgramNotification := func(change ProgramChange) error {
		if sentProgramNotification {
			if err := m.discord.SendProgramDivider(ctx, m.cfg.ProgramWebhookURL); err != nil {
				return fmt.Errorf("send program divider: %w", err)
			}
		}
		if err := m.discord.SendProgram(ctx, m.cfg.ProgramWebhookURL, change); err != nil {
			return err
		}
		sentProgramNotification = true
		return nil
	}

	for _, handle := range handles {
		next := current[handle]
		old, exists := previous.Programs[handle]
		if !exists {
			change := ProgramChange{Kind: "new", Handle: handle, After: &next}
			if err := sendProgramNotification(change); err != nil {
				log.Printf("notify new program %s: %v", handle, err)
				continue
			}
			if err := m.setProgram(handle, next); err != nil {
				return err
			}
			log.Printf("notified new program %s", handle)
			continue
		}
		details := diffProgram(old, next)
		if len(details) == 0 {
			continue
		}
		change := ProgramChange{Kind: "changed", Handle: handle, Details: details, Before: &old, After: &next}
		if err := sendProgramNotification(change); err != nil {
			log.Printf("notify changed program %s: %v", handle, err)
			continue
		}
		if err := m.setProgram(handle, next); err != nil {
			return err
		}
		log.Printf("notified %d changes for program %s", len(details), handle)
	}

	removed, err := m.store.UpdateProgramPresence(current)
	if err != nil {
		return err
	}
	for _, handle := range removed {
		log.Printf("program %s was absent from two complete checks; removed from baseline without notification", handle)
	}
	return nil
}

func (m *Monitor) setProgram(handle string, snapshot ProgramSnapshot) error {
	return m.store.SetProgram(handle, snapshot)
}

func (m *Monitor) CheckReports(ctx context.Context) error {
	summaries, err := m.h1.Reports(ctx)
	if err != nil {
		return fmt.Errorf("list reports: %w", err)
	}
	currentSummaries := make(map[string]json.RawMessage, len(summaries))
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		id, err := resourceID(summary)
		if err != nil {
			return fmt.Errorf("decode listed report: %w", err)
		}
		currentSummaries[id] = summary
		ids = append(ids, id)
	}
	sort.Strings(ids)
	previous, err := m.store.Snapshot()
	if err != nil {
		return fmt.Errorf("read report state: %w", err)
	}

	if !previous.ReportsInitialized {
		baseline := make(map[string]ReportSnapshot, len(ids))
		for _, id := range ids {
			report, err := m.h1.Report(ctx, id)
			if err != nil {
				return fmt.Errorf("fetch report %s for baseline: %w", id, err)
			}
			baseline[id] = ReportSnapshot{ID: id, Summary: currentSummaries[id], Report: report, CapturedAt: time.Now().UTC()}
		}
		if err := m.store.InitializeReports(baseline); err != nil {
			return err
		}
		log.Printf("report baseline created with %d reports; no Discord messages sent", len(baseline))
		return nil
	}

	for _, id := range ids {
		summary := currentSummaries[id]
		old, exists := previous.Reports[id]
		if exists && snapshotDigest(old.Summary) == snapshotDigest(summary) {
			continue
		}
		report, err := m.h1.Report(ctx, id)
		if err != nil {
			log.Printf("fetch changed report %s: %v", id, err)
			continue
		}
		next := ReportSnapshot{ID: id, Summary: summary, Report: report, CapturedAt: time.Now().UTC()}
		change := ReportChange{Kind: "new", ID: id, After: &next}
		if exists {
			next.DiscordThreadID = old.DiscordThreadID
			change.Kind = "changed"
			change.Before = &old
			change.Details = diffReport(old, next)
			if len(change.Details) == 0 {
				if err := m.setReport(id, next); err != nil {
					return err
				}
				continue
			}
		}
		if len(reportNotificationMessages(change, m.cfg.ReportNotificationMode, m.cfg.ReportNotifyOwnComments)) == 0 {
			if err := m.setReport(id, next); err != nil {
				return err
			}
			continue
		}
		threadID, err := m.discord.SendReport(ctx, m.cfg.ReportWebhookURL, change, m.cfg.ReportNotificationMode, m.cfg.ReportNotifyOwnComments, m.cfg.ReportThreadsEnabled)
		if err != nil {
			if exists && threadID != "" && threadID != old.DiscordThreadID {
				old.DiscordThreadID = threadID
				if stateErr := m.setReport(id, old); stateErr != nil {
					return stateErr
				}
			}
			log.Printf("notify report %s: %v", id, err)
			continue
		}
		next.DiscordThreadID = threadID
		if err := m.setReport(id, next); err != nil {
			return err
		}
		log.Printf("notified %s report %s", change.Kind, id)
	}
	return nil
}

func (m *Monitor) setReport(id string, snapshot ReportSnapshot) error {
	return m.store.SetReport(id, snapshot)
}

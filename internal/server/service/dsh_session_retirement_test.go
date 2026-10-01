package service

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// TestRetireDshSdkSessionsMigrationPostgres applies the shipped migrations to a
// throwaway database, seeds the SDK-era dsh sessions the retirement targets, then
// applies the retirement and pins what it changed: every dsh session carrying a
// provider session id is closed, and nothing else moves.
func TestRetireDshSdkSessionsMigrationPostgres(t *testing.T) {
	admin := taskSubmitTestPool(t)
	ctx := context.Background()
	name := "solo_retire_dsh_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})

	u, err := url.Parse(admin.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}

	exec(`CREATE TABLE schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`)
	files, err := filepath.Glob("../../../migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	migrate := func(lo, hi int) {
		t.Helper()
		for _, path := range files {
			version, err := strconv.Atoi(filepath.Base(path)[:6])
			if err != nil || version < lo || version > hi {
				continue
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			exec(string(body))
			exec(`INSERT INTO schema_migrations(version) VALUES($1) ON CONFLICT DO NOTHING`,
				strings.TrimSuffix(filepath.Base(path), ".up.sql"))
		}
	}
	// Everything before the retirement, so the seeded rows look pre-migration.
	migrate(1, 79)

	owner, agentID := uuid.NewString(), uuid.NewString()
	hash, err := bcrypt.GenerateFromPassword([]byte("DshAcp-2026!"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO users(id,email,display_name,password_hash) VALUES($1,'dsh-acp-retire@solo.local','DSH ACP 退役验证',$2)`,
		owner, string(hash))
	exec(`INSERT INTO agents(id,name,owner_id,model_provider,model_name) VALUES($1,'DshAcpRetire',$2,'dsh','deepseek-v4-flash')`,
		agentID, owner)

	externalID := func() string { return uuid.NewString() }
	seed := func(provider string, external *string, status string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO agent_sessions (agent_id, provider, external_session_id, status)
			VALUES ($1, $2, $3, $4)
			RETURNING id::text`, agentID, provider, external, status).Scan(&id); err != nil {
			t.Fatalf("seed %s session with status %s: %v", provider, status, err)
		}
		return id
	}
	sdkActive := seed("dsh", ptrTo(externalID()), "active")
	sdkRollover := seed("dsh", ptrTo(externalID()), "rollover_pending")
	alreadyClosed := seed("dsh", ptrTo(externalID()), "closed")
	withoutProviderID := seed("dsh", nil, "active")
	otherProvider := seed("claude", ptrTo(externalID()), "active")

	migrate(80, 80)

	statusOf := func(id string) string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read session %s: %v", id, err)
		}
		return status
	}
	if got := statusOf(sdkActive); got != AgentSessionStatusClosed {
		t.Fatalf("active dsh session status = %q, want closed", got)
	}
	if got := statusOf(sdkRollover); got != AgentSessionStatusClosed {
		t.Fatalf("rollover_pending dsh session status = %q, want closed", got)
	}
	if got := statusOf(alreadyClosed); got != AgentSessionStatusClosed {
		t.Fatalf("closed dsh session status = %q, want it left closed", got)
	}
	if got := statusOf(withoutProviderID); got != "active" {
		t.Fatalf("dsh session without a provider id status = %q, want it untouched", got)
	}
	if got := statusOf(otherProvider); got != "active" {
		t.Fatalf("claude session status = %q, want it untouched", got)
	}
}

func ptrTo(value string) *string { return &value }

// TestDshSessionRetirementColdStartsInsteadOfResuming pins the dispatch effect
// the retirement exists for: an active dsh session resumes, and the same row
// after retirement cold-starts with no resume id for the daemon.
func TestDshSessionRetirementColdStartsInsteadOfResuming(t *testing.T) {
	pool := agentRunTestPool(t)
	ctx := context.Background()
	svc := NewAgentRunService(pool)
	owner := agentRunUser(t, pool)
	agentID := agentRunAgent(t, pool, owner)
	channelID := agentRunChannel(t, pool, owner)

	session, err := svc.UpsertSession(ctx, UpsertSessionInput{
		AgentID: agentID, Provider: "dsh", ExternalSessionID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create dsh Session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_sessions WHERE id = $1`, session.ID)
	})

	resolve := func(resumeID string) SessionDispatch {
		t.Helper()
		run, err := svc.StartRun(ctx, StartRunInput{
			AgentID: agentID, SessionID: session.ID, TriggerType: AgentRunTriggerMessage,
			ChannelID: channelID, Status: AgentRunStatusQueued, Source: "dsh",
		})
		if err != nil {
			t.Fatalf("start dsh Run: %v", err)
		}
		dispatch, err := svc.ResolveSessionDispatch(ctx, ResolveSessionDispatchInput{
			RunID: run.ID, AgentID: agentID, ChannelID: channelID, Provider: "dsh",
			ResumeSessionID: resumeID,
		})
		if err != nil {
			t.Fatalf("resolve dsh dispatch: %v", err)
		}
		return dispatch
	}

	active := resolve(session.ExternalSessionID)
	if active.ColdStart || active.ResumeSessionID != session.ExternalSessionID {
		t.Fatalf("active dsh dispatch = %+v, want the stored session resumed", active)
	}

	// This is exactly what the retirement migration writes.
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'closed' WHERE id = $1`, session.ID); err != nil {
		t.Fatalf("retire the dsh Session: %v", err)
	}

	retired := resolve(session.ExternalSessionID)
	if !retired.ColdStart || retired.ResumeSessionID != "" {
		t.Fatalf("retired dsh dispatch = %+v, want a cold start without a resume id", retired)
	}
}

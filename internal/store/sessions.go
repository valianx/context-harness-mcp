package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSessionNotFound is returned by store functions when the requested
// session row does not exist. Callers map this to policy/session-not-found.
var ErrSessionNotFound = errors.New("session not found")

// Session is one row of the sessions table.
type Session struct {
	ID         string
	UserID     *string    // NULL when MCP_AUTH=none
	ProjectID  string
	WorkingDir *string
	StartedAt  time.Time
	EndedAt    *time.Time
	Summary    *string
}

// CreateSession inserts a new active session row and returns the new session
// UUID. userID may be nil (NULL when MCP_AUTH=none). projectID falls back to
// 'global' when empty.
func CreateSession(ctx context.Context, tx pgx.Tx, userID *string, projectID, workingDir string) (string, error) {
	if projectID == "" {
		projectID = "global"
	}

	var wdir *string
	if workingDir != "" {
		wdir = &workingDir
	}

	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, working_dir)
		 VALUES ($1::uuid, $2, $3)
		 RETURNING id`,
		userID, projectID, wdir,
	).Scan(&id)
	return id, err
}

// EndSession sets ended_at = now() and persists the optional summary. Calling
// it on an already-ended session is a no-op — the existing row is returned
// unchanged. Returns ErrSessionNotFound when no row exists for sessionID.
func EndSession(ctx context.Context, tx pgx.Tx, sessionID string, summary *string) (*Session, error) {
	// Single UPDATE … RETURNING with idempotent guard: if ended_at IS NULL,
	// update it; otherwise touch nothing and still return the row via the
	// RETURNING clause (using COALESCE keeps the existing value).
	//
	// The two-step approach (UPDATE then SELECT) is more readable but requires
	// two round-trips. The CASE expression below achieves idempotency in one:
	// ended_at = CASE WHEN ended_at IS NULL THEN now() ELSE ended_at END
	var s Session
	var userIDStr *string

	err := tx.QueryRow(ctx,
		`UPDATE sessions
		 SET ended_at = CASE WHEN ended_at IS NULL THEN now() ELSE ended_at END,
		     summary  = CASE WHEN ended_at IS NULL THEN $2   ELSE summary  END
		 WHERE id = $1
		 RETURNING id, user_id::text, project_id, working_dir, started_at, ended_at, summary`,
		sessionID, summary,
	).Scan(&s.ID, &userIDStr, &s.ProjectID, &s.WorkingDir, &s.StartedAt, &s.EndedAt, &s.Summary)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}

	s.UserID = userIDStr
	return &s, nil
}

// GetSession returns a single session by ID. Returns (nil, ErrSessionNotFound)
// when no row exists for sessionID.
func GetSession(ctx context.Context, pool *pgxpool.Pool, sessionID string) (*Session, error) {
	var s Session
	var userIDStr *string

	err := pool.QueryRow(ctx,
		`SELECT id, user_id::text, project_id, working_dir, started_at, ended_at, summary
		 FROM sessions
		 WHERE id = $1`,
		sessionID,
	).Scan(&s.ID, &userIDStr, &s.ProjectID, &s.WorkingDir, &s.StartedAt, &s.EndedAt, &s.Summary)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}

	s.UserID = userIDStr
	return &s, nil
}

// SessionNodeRow is a read-projection of a nodes row enriched for the
// session_summary response. Includes soft-deleted nodes intentionally — the
// session summary is a full audit trail of what the session created.
type SessionNodeRow struct {
	ID               string
	Name             string
	NodeType         string
	ProjectID        string
	CreatedAt        time.Time
	DeletedAt        *time.Time
	ObservationCount int
}

// ListSessionNodes returns all nodes (including soft-deleted) attached to the
// given session, ordered by created_at ASC. The deliberate inclusion of
// soft-deleted nodes lets session_summary show a full audit trail of the
// session's actions.
func ListSessionNodes(ctx context.Context, pool *pgxpool.Pool, sessionID string) ([]SessionNodeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT n.id, n.name, n.node_type, n.project_id, n.created_at, n.deleted_at,
		        (SELECT count(*)::int FROM observations o
		         WHERE o.node_id = n.id AND o.deleted_at IS NULL) AS observation_count
		 FROM nodes n
		 WHERE n.session_id = $1
		 ORDER BY n.created_at ASC, n.id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SessionNodeRow
	for rows.Next() {
		var r SessionNodeRow
		if err := rows.Scan(
			&r.ID, &r.Name, &r.NodeType, &r.ProjectID,
			&r.CreatedAt, &r.DeletedAt, &r.ObservationCount,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// CountSessionNodes returns the number of nodes (including soft-deleted)
// attached to a session. Used by session_end to populate the node_count field.
func CountSessionNodes(ctx context.Context, pool *pgxpool.Pool, sessionID string) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM nodes WHERE session_id = $1`,
		sessionID,
	).Scan(&n)
	return n, err
}

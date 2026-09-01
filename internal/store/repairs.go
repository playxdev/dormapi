package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalid marks input the caller got wrong, as opposed to a server fault.
var ErrInvalid = errors.New("store: invalid input")

// Categories a tenant may report. Anything else is rejected rather than stored,
// so the operator's filters cannot be polluted by arbitrary strings.
var repairCategories = map[string]bool{
	"electrical": true,
	"plumbing":   true,
	"aircon":     true,
	"furniture":  true,
	"other":      true,
}

type Repair struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Category  string `json:"category"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type RepairEvent struct {
	Status    string `json:"status"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

type RepairDetail struct {
	Repair
	Events []RepairEvent `json:"events"`
}

const repairSelect = `
	SELECT rr.id, rr.ref, rr.category, rr.title, rr.detail, rr.status,
	       rr.created_at, rr.updated_at
	FROM repair_requests rr
	JOIN tenancies t ON t.id = rr.tenancy_id AND t.status = 'active'
	WHERE t.user_id = ?1`

func (s *Store) RepairsForUser(ctx context.Context, userID string) ([]Repair, error) {
	res, err := s.db.Query(ctx, repairSelect+` ORDER BY rr.created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list repairs: %w", err)
	}

	repairs := make([]Repair, 0, len(res.Results))
	for _, row := range res.Results {
		repairs = append(repairs, scanRepair(row))
	}
	return repairs, nil
}

func (s *Store) RepairForUser(ctx context.Context, userID, repairID string) (*RepairDetail, error) {
	res, err := s.db.Query(ctx, repairSelect+` AND rr.id = ?2`, userID, repairID)
	if err != nil {
		return nil, fmt.Errorf("store: get repair: %w", err)
	}
	if len(res.Results) == 0 {
		return nil, ErrNotFound
	}

	events, err := s.db.Query(ctx, `
		SELECT status, note, created_at FROM repair_events
		WHERE repair_id = ?1 ORDER BY created_at`, repairID)
	if err != nil {
		return nil, fmt.Errorf("store: get repair events: %w", err)
	}

	detail := &RepairDetail{
		Repair: scanRepair(res.Results[0]),
		Events: make([]RepairEvent, 0, len(events.Results)),
	}
	for _, row := range events.Results {
		detail.Events = append(detail.Events, RepairEvent{
			Status:    text(row["status"]),
			Note:      text(row["note"]),
			CreatedAt: text(row["created_at"]),
		})
	}
	return detail, nil
}

// CreateRepair files a request against the caller's active tenancy.
//
// The tenancy, property and room all come from the database via the user ID.
// Nothing about where the request is filed is taken from the request body.
func (s *Store) CreateRepair(ctx context.Context, userID, category, title, detail string) (*Repair, error) {
	title = strings.TrimSpace(title)
	detail = strings.TrimSpace(detail)

	if title == "" || len([]rune(title)) > 120 {
		return nil, fmt.Errorf("%w: title", ErrInvalid)
	}
	if len([]rune(detail)) > 2000 {
		return nil, fmt.Errorf("%w: detail", ErrInvalid)
	}
	if !repairCategories[category] {
		return nil, fmt.Errorf("%w: category", ErrInvalid)
	}

	id := uuid.NewString()

	now := time.Now()
	period := fmt.Sprintf("%04d-%02d", now.Year(), int(now.Month()))
	// Buddhist-era year and month, matching the reference tenants quote to staff.
	refPrefix := fmt.Sprintf("R-%02d%02d", (now.Year()+543)%100, int(now.Month()))

	// One statement, because D1 allows no parameterised multi-statement write.
	// The sequence is chosen by a subquery inside the INSERT, so the read and
	// the write cannot be separated. Two concurrent requests can still pick the
	// same number; UNIQUE (property_id, period, seq) rejects the loser rather
	// than issuing a duplicate reference, and the caller retries.
	//
	// No creation row is written to repair_events: it would duplicate
	// created_at. The history records changes of status, not the first one.
	_, err := s.db.Query(ctx, `
		INSERT INTO repair_requests
			(id, property_id, room_id, tenancy_id, period, seq, ref, category, title, detail)
		SELECT ?1, t.property_id, t.room_id, t.id, ?2,
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM repair_requests
			 WHERE property_id = t.property_id AND period = ?3),
			?4 || printf('%03d',
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM repair_requests
			 WHERE property_id = t.property_id AND period = ?5)),
			?6, ?7, ?8
		FROM tenancies t
		WHERE t.user_id = ?9 AND t.status = 'active'
		LIMIT 1`,
		id, period, period, refPrefix, period, category, title, detail, userID)
	if err != nil {
		return nil, fmt.Errorf("store: create repair: %w", err)
	}

	res, err := s.db.Query(ctx, repairSelect+` AND rr.id = ?2`, userID, id)
	if err != nil {
		return nil, fmt.Errorf("store: read back repair: %w", err)
	}
	if len(res.Results) == 0 {
		// The INSERT ... SELECT matched no tenancy, so it wrote nothing: the
		// user has no active room to report against.
		return nil, ErrNotFound
	}

	repair := scanRepair(res.Results[0])
	return &repair, nil
}

func scanRepair(row map[string]any) Repair {
	return Repair{
		ID:        text(row["id"]),
		Ref:       text(row["ref"]),
		Category:  text(row["category"]),
		Title:     text(row["title"]),
		Detail:    text(row["detail"]),
		Status:    text(row["status"]),
		CreatedAt: text(row["created_at"]),
		UpdatedAt: text(row["updated_at"]),
	}
}

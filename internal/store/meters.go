package store

import (
	"context"
	"fmt"
)

// Meter is one recorded reading: what the meter said last time, what it says
// now, and the difference the tenant is billed for.
//
// Values are whole units — cubic metres of water, kilowatt-hours of
// electricity — as the meter face shows them.
type Meter struct {
	Period     string `json:"period"`
	Kind       string `json:"kind"`
	Previous   int64  `json:"previous"`
	Current    int64  `json:"current"`
	Used       int64  `json:"used"`
	RecordedAt string `json:"recorded_at"`
	RoomNumber string `json:"room_number,omitempty"`
}

// MetersForUser lists the readings the caller is entitled to see.
//
// A reading belongs to a room, and a room outlives a tenancy. Bounding each
// reading by the contract's own dates is what stops a tenant who moved in last
// month from reading the previous occupant's consumption — the join alone
// would hand it over.
//
// Skipped rooms are excluded: a walk that passed a door without reading it has
// no number to show, and rendering it as zero usage would be a lie.
func (s *Store) MetersForUser(ctx context.Context, userID string) ([]Meter, error) {
	res, err := s.db.Query(ctx, `
		SELECT m.period, m.kind, m.prev_value, m.value,
		       m.value - m.prev_value AS used,
		       m.recorded_at, r.number AS room_number
		FROM meter_readings m
		JOIN rooms r     ON r.id = m.room_id
		JOIN contracts c ON c.room_id = r.id
		JOIN tenants t   ON t.id = c.tenant_id
		WHERE t.user_id = ?1
		  AND m.status = 'recorded'
		  AND m.period >= substr(c.start_date, 1, 7)
		  AND (c.end_date IS NULL OR m.period <= substr(c.end_date, 1, 7))
		GROUP BY m.id
		ORDER BY m.period DESC, m.kind`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list meters: %w", err)
	}

	meters := make([]Meter, 0, len(res.Results))
	for _, row := range res.Results {
		meters = append(meters, Meter{
			Period:     text(row["period"]),
			Kind:       text(row["kind"]),
			Previous:   number(row["prev_value"]),
			Current:    number(row["value"]),
			Used:       number(row["used"]),
			RecordedAt: text(row["recorded_at"]),
			RoomNumber: text(row["room_number"]),
		})
	}
	return meters, nil
}

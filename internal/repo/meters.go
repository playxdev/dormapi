package repo

import (
	"context"
	"fmt"
)

// Meter is one recorded reading: what the meter said last time, what it says
// now, and the difference the resident is billed for.
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

// Meters lists the readings the caller is entitled to see.
//
// `meter_reading.contract_id` is the whole of the authorisation: a reading
// belongs to a room, a room outlives a tenancy, and the previous occupant's
// consumption is not this resident's to read. The pre-XYZ schema had no such
// column and this query bounded the readings by the lease's own dates instead;
// the column is exact where the date range was an approximation of it.
//
// Readings taken while the room was vacant carry no contract_id and are
// therefore invisible here, which is correct.
//
// PENDING and SKIPPED rows are excluded: a walk that passed a door without
// reading it has no number to show, and rendering it as zero usage would be a
// lie. CONFIRMED means a staff member looked at an odd reading and stood by
// it, so it counts.
func (r *Repo) Meters(ctx context.Context, t *Tenancy) ([]Meter, error) {
	res, err := r.db.Query(ctx, `
		SELECT m.period, m.kind, m.prev_value, m.value,
		       m.value - m.prev_value AS used,
		       m.recorded_at, rm.number AS room_number
		FROM meter_reading m
		JOIN contract c ON c.tenant_id = m.tenant_id AND c.contract_id = m.contract_id
		JOIN room rm    ON rm.tenant_id = m.tenant_id AND rm.room_id = m.room_id
		WHERE m.tenant_id = ?1
		  AND c.party_id = ?2
		  AND m.status IN ('RECORDED', 'CONFIRMED')
		  AND m.deleted_at IS NULL
		  AND c.deleted_at IS NULL
		ORDER BY m.period DESC, m.kind, m.reading_id`, t.tenantID, t.partyID)
	if err != nil {
		return nil, fmt.Errorf("repo: list meters: %w", err)
	}

	meters := make([]Meter, 0, len(res.Results))
	for _, row := range res.Results {
		meters = append(meters, Meter{
			Period:     text(row["period"]),
			Kind:       toWire(text(row["kind"])),
			Previous:   number(row["prev_value"]),
			Current:    number(row["value"]),
			Used:       number(row["used"]),
			RecordedAt: text(row["recorded_at"]),
			RoomNumber: text(row["room_number"]),
		})
	}
	return meters, nil
}

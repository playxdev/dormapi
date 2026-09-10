package repo

import "strings"

// The wire vocabulary is lowercase; the XYZ schema's is uppercase.
//
// `ticket.status` is OPEN in the database and "open" on the wire, and the same
// for priorities, meter kinds and payment methods. The pre-XYZ schema stored
// the lowercase form, so the deployed MINI App matches on it — see
// `mini/src/pages/repairs.js`, whose label maps are keyed exactly that way.
//
// Translating here rather than renaming the contract follows the rule this
// service already states in its README: the wire keeps the names the design
// document uses, even where the schema has since chosen others. Nothing is
// gained by making every installed client wrong about a chip's colour, and the
// mapping is two functions in one file rather than a special case in each
// query.
//
// Values are mapped, never lowercased blindly on the way in: a client may send
// anything, and each writing path validates against its own fixed set before
// this is reached.

func toWire(v string) string { return strings.ToLower(v) }

func toSchema(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }

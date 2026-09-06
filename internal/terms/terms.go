// Package terms names the documents a confirmation is taken to cover.
//
// The text itself is not here. It is rendered by the backoffice from
// `src/routes/contract-print.tsx` — clauses 1 to 7 are the lease, clause 8 is
// the PDPA notice — because that template interpolates the contract's own
// numbers and cannot be a static file.
//
// Only the version travels, because only the version has to be recorded
// against a confirmation. It answers "which document did this person agree
// to?" months later, which the money snapshot alone cannot.
//
// Bump a version in the same change that edits the matching clauses in
// dormplace. A version naming a template that never existed is worse than no
// version at all.
package terms

const (
	// LeaseVersion covers clauses 1 to 7 of the tenancy agreement.
	LeaseVersion = "1.0"

	// PDPAVersion covers clause 8, the personal-data notice.
	PDPAVersion = "1.0"
)

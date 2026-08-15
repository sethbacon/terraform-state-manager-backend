package repositories

// Completeness carries the drift contract's five markers — what a check did NOT
// do. It is ONE definition, embedded by every shape that receives, stores or
// returns them: the two wire DTOs, the Detection bound for a record, and both
// the drift_records and drift_runs rows.
//
// Shared rather than repeated because the whole defect class here is a layer
// quietly dropping a marker: the producers computed all five (#379) while both
// receivers dropped them (#382), and #382 in turn landed them on records while
// drift_runs kept none. Each of those was a hand-copy of the vocabulary that
// fell behind. With one struct, adding the sixth marker reaches every layer at
// once or fails to compile.
//
// Emitted unconditionally (no omitempty): "false" here is the positive claim
// that the check was complete, which is exactly the thing a consumer must not
// have to infer from an absent field.
type Completeness struct {
	Truncated      bool `json:"truncated"`
	OmittedEntries int  `json:"omitted_entries"`
	OmittedAttrs   int  `json:"omitted_attrs"`
	Unparseable    bool `json:"unparseable"`
	Unmasked       bool `json:"unmasked"`
}

// MarkTruncation widens Truncated to agree with the omission counts. The flag is
// only ever widened, never narrowed: a producer that reports omissions but
// forgets the flag is repaired, while one that reports the flag with no counts
// (bounded by something it could not count) is believed. Under-reporting a bound
// is the direction that misleads a consumer into reading an absent resource as
// evidence of absence.
//
// Applied on both storage paths, so one callback cannot leave a run row and its
// record row disagreeing about whether the same check was bounded.
func (c *Completeness) MarkTruncation() {
	if c.OmittedEntries > 0 || c.OmittedAttrs > 0 {
		c.Truncated = true
	}
}

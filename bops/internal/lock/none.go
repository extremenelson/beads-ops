package lock

// OffWarning is what every command says when lock.mode is none, so nobody
// takes an unprotected claim for a protected one.
const OffWarning = "lock.mode is none: claims are decided by this machine's ledger alone, with no claim ref; two machines can both claim one bead"

// DoctorOff is the diagnosis when lock.mode is none. There is no lock, so
// nothing on origin is checked: every check is skipped and none fails.
func DoctorOff() *Diagnosis {
	d := &Diagnosis{Checks: []Check{}, Failed: []string{}, Warnings: []string{OffWarning}}
	for _, id := range []string{"origin", "fetch", "push", "lock"} {
		d.Checks = append(d.Checks, Check{ID: id, Status: CheckSkip,
			Detail: "not run: lock.mode is none (set in .bops.json), so claims use no git ref and no remote"})
	}
	return d
}

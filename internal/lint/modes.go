package lint

import (
	"fmt"
	"strings"
)

// Mode is a PostgreSQL table-level lock mode, ordered from weakest to
// strongest. None means the statement takes no lock on the table.
type Mode int

const (
	None Mode = iota
	AccessShare
	RowShare
	RowExclusive
	ShareUpdateExclusive
	Share
	ShareRowExclusive
	Exclusive
	AccessExclusive
)

var modeNames = [...]string{"none", "ACCESS SHARE", "ROW SHARE", "ROW EXCLUSIVE",
	"SHARE UPDATE EXCLUSIVE", "SHARE", "SHARE ROW EXCLUSIVE", "EXCLUSIVE", "ACCESS EXCLUSIVE"}

func (m Mode) String() string { return modeNames[m] }

// pg_locks spells modes as "AccessExclusiveLock".
var pgLockNames = map[string]Mode{
	"AccessShareLock": AccessShare, "RowShareLock": RowShare, "RowExclusiveLock": RowExclusive,
	"ShareUpdateExclusiveLock": ShareUpdateExclusive, "ShareLock": Share,
	"ShareRowExclusiveLock": ShareRowExclusive, "ExclusiveLock": Exclusive,
	"AccessExclusiveLock": AccessExclusive,
}

// ParsePGLock converts a pg_locks mode name.
func ParsePGLock(s string) (Mode, bool) {
	m, ok := pgLockNames[s]
	return m, ok
}

// ParseMode reads a mode as written in LOCK TABLE ... IN <mode> MODE.
func ParseMode(s string) (Mode, error) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), " "))
	for m, n := range modeNames {
		if n == s && m > 0 {
			return Mode(m), nil
		}
	}
	return None, fmt.Errorf("unknown lock mode %q", s)
}

// conflicts[a] is the set of modes a conflicts with, from the table in the
// PostgreSQL manual, "Explicit Locking".
var conflicts = map[Mode][]Mode{
	AccessShare:          {AccessExclusive},
	RowShare:             {Exclusive, AccessExclusive},
	RowExclusive:         {Share, ShareRowExclusive, Exclusive, AccessExclusive},
	ShareUpdateExclusive: {ShareUpdateExclusive, Share, ShareRowExclusive, Exclusive, AccessExclusive},
	Share:                {RowExclusive, ShareUpdateExclusive, ShareRowExclusive, Exclusive, AccessExclusive},
	ShareRowExclusive:    {RowExclusive, ShareUpdateExclusive, Share, ShareRowExclusive, Exclusive, AccessExclusive},
	Exclusive:            {RowShare, RowExclusive, ShareUpdateExclusive, Share, ShareRowExclusive, Exclusive, AccessExclusive},
	AccessExclusive:      {AccessShare, RowShare, RowExclusive, ShareUpdateExclusive, Share, ShareRowExclusive, Exclusive, AccessExclusive},
}

// Conflicts reports whether holding a blocks a request for b.
func Conflicts(a, b Mode) bool {
	for _, m := range conflicts[a] {
		if m == b {
			return true
		}
	}
	return false
}

// BlocksReads reports whether m blocks plain SELECTs (ACCESS SHARE).
func (m Mode) BlocksReads() bool { return Conflicts(m, AccessShare) }

// BlocksWrites reports whether m blocks INSERT/UPDATE/DELETE (ROW EXCLUSIVE).
func (m Mode) BlocksWrites() bool { return Conflicts(m, RowExclusive) }

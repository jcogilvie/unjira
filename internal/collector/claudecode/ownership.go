package claudecode

// ownership.go decides which root transcript a copied line belongs to (F33).
//
// A resumed Claude Code session writes a new transcript beginning with a copy of earlier
// history. Measured across the local transcripts: 1,841 tool_use ids appear in more than
// one root transcript, in 7 groups of files, and every copied line keeps its `uuid` and
// timestamp; only `sessionId` is rewritten. Segment ExternalIDs are per file, so the copy
// became a second set of segment events: the same work, reaching clustering twice.
//
// So every line with a uuid has exactly one owner among the root transcripts holding it,
// and a transcript segments only the lines it owns. The owner is the holder whose LAST
// line is earliest, ties broken by path. The original stops when it is resumed, so its
// last line is frozen before the copy's, and a copy made later can never take ownership
// from it. Ownership is decided over every root transcript, including those outside the
// backfill window: an original too old to collect still owns its lines, which are older
// than the window too.
//
// Not filtered by ownership: lines without a uuid (Claude Code's metadata lines, and
// every transcript that predates the field), PR anchors (keyed by tool_use id, so a copy
// already dedupes, and a call whose result landed in the copy must still resolve), and
// subagent transcripts (keyed by agent id, so a byte-identical copy dedupes).

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// lineOwners maps a line uuid to the path of the root transcript that owns it.
type lineOwners map[string]string

// owns reports whether the transcript at path owns line. A line without a uuid, or one no
// index was built for, belongs to whoever holds it.
func (o lineOwners) owns(path string, line map[string]any) bool {
	uuid, _ := line["uuid"].(string)
	if uuid == "" {
		return true
	}
	owner, ok := o[uuid]

	return !ok || owner == path
}

// buildLineOwners reads every root transcript's line uuids and last timestamp, and assigns
// each uuid to its owner.
func buildLineOwners(transcripts []transcript) (lineOwners, error) {
	type holder struct {
		path, lastTS string
		uuids        []string
	}

	var holders []holder
	for _, t := range transcripts {
		if t.sub != nil {
			continue
		}
		h, err := readUUIDs(t.path)
		if err != nil {
			return nil, err
		}
		holders = append(holders, holder{path: t.path, lastTS: h.lastTS, uuids: h.uuids})
	}

	// Timestamps are Claude Code's fixed-width RFC 3339 UTC form, so lexical order is
	// chronological.
	slices.SortFunc(holders, func(a, b holder) int {
		return cmp.Or(cmp.Compare(a.lastTS, b.lastTS), cmp.Compare(a.path, b.path))
	})

	owners := lineOwners{}
	for _, h := range holders {
		for _, u := range h.uuids {
			if _, taken := owners[u]; !taken {
				owners[u] = h.path
			}
		}
	}

	return owners, nil
}

type uuidScan struct {
	uuids  []string
	lastTS string
}

// readUUIDs reads only each line's uuid and timestamp. A line that is not JSON is skipped,
// as jsonlLines skips it.
func readUUIDs(path string) (uuidScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return uuidScan{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out uuidScan
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var line struct {
			UUID      string `json:"uuid"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		if line.UUID != "" {
			out.uuids = append(out.uuids, line.UUID)
		}
		if line.Timestamp > out.lastTS {
			out.lastTS = line.Timestamp
		}
	}
	if err := scanner.Err(); err != nil {
		return uuidScan{}, fmt.Errorf("reading %s: %w", path, err)
	}

	return out, nil
}

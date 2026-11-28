package portalconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// cjNode is one synthetic member. The fields are the identity the loader stores.
func cjNode(bridge, id string) CertifiedNode {
	return CertifiedNode{Bridge: bridge, ID: id}
}

func cjPair() (CertifiedNode, CertifiedNode) {
	return cjNode("bridge.a", "resource:member-a"), cjNode("bridge.b", "resource:member-b")
}

func cjJoin(action string, a, b, survivor CertifiedNode, revision, target string) CertifiedJoin {
	return CertifiedJoin{Action: action, A: a, B: b, Survivor: survivor, Revision: revision, TargetRevision: target}
}

func cjWrite(t *testing.T, root, revision string, records []OperatorRecord, joins []CertifiedJoin) {
	t.Helper()
	if records == nil {
		records = []OperatorRecord{}
	}
	raw, err := json.Marshal(operatorRecordDocument{Format: OperatorRecordsFormat, Revision: revision, Records: records, CertifiedJoins: joins})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, OperatorRecordsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cjLoad(t *testing.T, root string) []TrustedCertifiedJoin {
	t.Helper()
	joins, err := LoadTrustedCertifiedJoins(root)
	if err != nil {
		t.Fatal(err)
	}
	return joins
}

// TestCertifiedJoinLoaderVariesEveryStatementField checks each field of a
// trusted statement. The production mutation named with the field is the one
// a wrong loader would hide.
func TestCertifiedJoinLoaderVariesEveryStatementField(t *testing.T) {
	a, b := cjPair()
	base := cjJoin(CertifiedJoinCertify, a, b, a, "1", "")
	third := cjNode("bridge.c", "resource:member-c")

	// action selects the statement action. Folding it into the digest would
	// make a revoke of the same pair, survivor and revision unequal; storing
	// certify for every action would keep the action equal.
	t.Run("action", func(t *testing.T) {
		root := t.TempDir()
		revoke := cjJoin(CertifiedJoinRevoke, a, b, a, "1", "1")
		cjWrite(t, root, "0", nil, []CertifiedJoin{base, revoke})
		loaded := cjLoad(t, root)
		if len(loaded) != 2 || loaded[0].Join.Action != CertifiedJoinCertify || loaded[1].Join.Action != CertifiedJoinRevoke {
			t.Fatalf("actions %+v", loaded)
		}
		if loaded[0].Evidence != CertifiedJoinEvidence(base) || loaded[1].Evidence != CertifiedJoinEvidence(revoke) || loaded[0].Evidence != loaded[1].Evidence {
			t.Fatalf("action changed the origin evidence: %+v %+v", loaded[0].Evidence, loaded[1].Evidence)
		}
	})

	// a.bridge, a.id, b.bridge and b.id are the pair. Omitting one from the
	// digest would keep the starting evidence. The survivor stays the member
	// that the mutation does not move, so only that one field changes.
	survivorA := cjJoin(CertifiedJoinCertify, a, b, a, "1", "")
	survivorB := cjJoin(CertifiedJoinCertify, a, b, b, "1", "")
	for _, mutate := range []struct {
		name   string
		start  CertifiedJoin
		mutate func(*CertifiedJoin)
	}{
		{"a.bridge", survivorB, func(j *CertifiedJoin) { j.A.Bridge = "bridge.other" }},
		{"a.id", survivorB, func(j *CertifiedJoin) { j.A.ID = "resource:member-other" }},
		{"b.bridge", survivorA, func(j *CertifiedJoin) { j.B.Bridge = "bridge.other" }},
		{"b.id", survivorA, func(j *CertifiedJoin) { j.B.ID = "resource:member-other" }},
		{"survivor", survivorA, func(j *CertifiedJoin) { j.Survivor = b }},
		{"revision", survivorA, func(j *CertifiedJoin) { j.Revision = "2" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			changed := mutate.start
			mutate.mutate(&changed)
			root := t.TempDir()
			cjWrite(t, root, "0", nil, []CertifiedJoin{changed})
			loaded := cjLoad(t, root)
			if len(loaded) != 1 || loaded[0].Join != changed {
				t.Fatalf("stored %+v, want %+v", loaded, changed)
			}
			if loaded[0].Evidence != CertifiedJoinEvidence(changed) || loaded[0].Evidence == CertifiedJoinEvidence(mutate.start) {
				t.Fatalf("%s did not change the origin evidence", mutate.name)
			}
		})
	}

	// target_revision is stored on a revoke and is not part of the origin
	// evidence. Folding it into the digest would make the two evidences differ;
	// dropping it would make the stored targets equal.
	t.Run("target_revision", func(t *testing.T) {
		first := cjJoin(CertifiedJoinRevoke, a, b, a, "1", "1")
		second := cjJoin(CertifiedJoinRevoke, a, b, a, "1", "2")
		left, right := t.TempDir(), t.TempDir()
		cjWrite(t, left, "0", nil, []CertifiedJoin{first})
		cjWrite(t, right, "0", nil, []CertifiedJoin{second})
		gotLeft, gotRight := cjLoad(t, left), cjLoad(t, right)
		if gotLeft[0].Join.TargetRevision == gotRight[0].Join.TargetRevision {
			t.Fatal("target revision was not stored")
		}
		if gotLeft[0].Evidence != gotRight[0].Evidence || gotLeft[0].Evidence != CertifiedJoinEvidence(first) {
			t.Fatalf("target revision changed the origin evidence: %+v %+v", gotLeft[0].Evidence, gotRight[0].Evidence)
		}
	})

	// The ordinary section revision is not part of the origin evidence.
	t.Run("section revision", func(t *testing.T) {
		left, right := t.TempDir(), t.TempDir()
		cjWrite(t, left, "1", nil, []CertifiedJoin{base})
		cjWrite(t, right, "2", nil, []CertifiedJoin{base})
		if cjLoad(t, left)[0].Evidence != cjLoad(t, right)[0].Evidence {
			t.Fatal("the section revision changed the origin evidence")
		}
		setLeft, err := LoadOperatorRecords(left)
		if err != nil {
			t.Fatal(err)
		}
		setRight, err := LoadOperatorRecords(right)
		if err != nil {
			t.Fatal(err)
		}
		if setLeft.Revision == setRight.Revision {
			t.Fatal("the section revision was not loaded")
		}
	})

	rejected := []struct {
		name, field string
		join        CertifiedJoin
		extra       string
	}{
		{"a equals b", "certified_joins[0].b", cjJoin(CertifiedJoinCertify, a, a, a, "1", ""), ""},
		{"survivor outside the pair", "certified_joins[0].survivor", cjJoin(CertifiedJoinCertify, a, b, third, "1", ""), ""},
		{"revision 00", "certified_joins[0].revision", cjJoin(CertifiedJoinCertify, a, b, a, "00", ""), ""},
		{"target on certify", "certified_joins[0]", cjJoin(CertifiedJoinCertify, a, b, a, "1", "1"), ""},
		{"revoke without target", "certified_joins[0]", cjJoin(CertifiedJoinRevoke, a, b, a, "1", ""), ""},
		{"unknown action", "certified_joins[0].action", cjJoin("alias", a, b, a, "1", ""), ""},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cjWrite(t, root, "0", nil, []CertifiedJoin{tc.join})
			_, err := LoadTrustedCertifiedJoins(root)
			requireField(t, err, "invalid_record", tc.field)
		})
	}

	// A presented digest, evidence, statement or origin is not the loader's
	// evidence. Accepting the member would certify the join from the file.
	for _, member := range []string{"digest", "evidence", "statement", "origin"} {
		t.Run(member, func(t *testing.T) {
			root := t.TempDir()
			cjWrite(t, root, "0", nil, []CertifiedJoin{base})
			path := filepath.Join(root, OperatorRecordsFile)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Replace(string(raw), `"revision":"1"`, `"revision":"1","`+member+`":"present"`, 1)
			if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = LoadTrustedCertifiedJoins(root)
			requireField(t, err, "invalid_record", "certified_joins[0]")
		})
	}
}

// Ordinary replacement writes the accepted host-local section back. A failed
// replacement before the rename keeps that section and its evidence.
func TestCertifiedJoinWriteBackPreservesTheAcceptedSection(t *testing.T) {
	a, b := cjPair()
	join := cjJoin(CertifiedJoinCertify, a, b, b, "4", "")
	evidence := CertifiedJoinEvidence(join)
	s, root := openTest(t)
	anchor := oprAnchor(oprHome, "resource:portal-opr:array-1", "resource.pv_array", "array-south")
	cjWrite(t, root, "0", []OperatorRecord{anchor}, []CertifiedJoin{join})

	current, err := s.OperatorRecords()
	if err != nil || len(current.Records) != 1 || current.Revision != "0" {
		t.Fatalf("ordinary section %+v %v", current, err)
	}
	if loaded := cjLoad(t, root); len(loaded) != 1 || loaded[0].Join != join || loaded[0].Evidence != evidence {
		t.Fatalf("loaded %+v", loaded)
	}

	s.fault = func(name string) error {
		if name == "create" {
			return os.ErrPermission
		}
		return nil
	}
	changed := current
	changed.Records = []OperatorRecord{}
	if _, err = s.ReplaceOperatorRecords(changed); err == nil {
		t.Fatal("the create fault replaced the section")
	}
	requireCode(t, err, "persistence_failed")
	s.fault = nil
	if loaded := cjLoad(t, root); len(loaded) != 1 || loaded[0].Join != join || loaded[0].Evidence != evidence {
		t.Fatalf("the failed replacement changed the host-local section: %+v", loaded)
	}
	if read, err := s.OperatorRecords(); err != nil || !reflect.DeepEqual(read, current) {
		t.Fatalf("the failed replacement changed the ordinary section: %+v %v", read, err)
	}

	saved, err := s.ReplaceOperatorRecords(changed)
	if err != nil || saved.Revision != "1" || len(saved.Records) != 0 {
		t.Fatalf("clear %+v %v", saved, err)
	}
	if loaded := cjLoad(t, root); len(loaded) != 1 || loaded[0].Join != join || loaded[0].Evidence != evidence {
		t.Fatalf("clearing the ordinary section changed the host-local section: %+v", loaded)
	}
	restored := saved
	restored.Records = []OperatorRecord{anchor}
	if saved, err = s.ReplaceOperatorRecords(restored); err != nil || saved.Revision != "2" || len(saved.Records) != 1 {
		t.Fatalf("restore %+v %v", saved, err)
	}
	if loaded := cjLoad(t, root); len(loaded) != 1 || loaded[0].Join != join || loaded[0].Evidence != evidence {
		t.Fatalf("changing the ordinary section changed the host-local section: %+v", loaded)
	}
	set, err := LoadOperatorRecords(root)
	if err != nil || set.Revision != "2" || len(set.Records) != 1 {
		t.Fatalf("ordinary load %+v %v", set, err)
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "certified_joins") || strings.Contains(string(raw), "target_revision") {
		t.Fatalf("the ordinary set carries the host-local section: %s", raw)
	}
}

func TestTrustedCertifiedJoinsMissingFileIsEmpty(t *testing.T) {
	joins, err := LoadTrustedCertifiedJoins("")
	if err != nil || joins != nil {
		t.Fatalf("empty root %+v %v", joins, err)
	}
	root := t.TempDir()
	joins, err = LoadTrustedCertifiedJoins(root)
	if err != nil || joins != nil {
		t.Fatalf("missing file %+v %v", joins, err)
	}
}

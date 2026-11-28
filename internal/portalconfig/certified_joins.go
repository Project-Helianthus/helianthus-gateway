package portalconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// Certified-join actions. A revocation names the certification it revokes; it
// does not carry a new origin.
const (
	CertifiedJoinCertify = "certify"
	CertifiedJoinRebind  = "rebind"
	CertifiedJoinRevoke  = "revoke"
)

// certifiedJoinEvidenceDomain separates origin evidence of a trusted join from
// the ordinary section's audit. The digest covers the pair, the survivor and
// the statement revision only: a revocation repeats that evidence and names
// the certification revision beside it, and an ordinary section replacement
// must not change it. The file cannot supply this digest.
const certifiedJoinEvidenceDomain = "helianthus.portal-config.certified-join.v1\x00"

// CertifiedNode is one member of a trusted join, named by the bridge and the
// resource the host allocated.
type CertifiedNode struct {
	Bridge string `json:"bridge"`
	ID     string `json:"id"`
}

// CertifiedJoin is one host-local statement. TargetRevision is set only on a
// revocation and is the revision of the certification it revokes. The value
// has no origin of its own: the trusted loader binds that.
type CertifiedJoin struct {
	Action         string        `json:"action"`
	A              CertifiedNode `json:"a"`
	B              CertifiedNode `json:"b"`
	Survivor       CertifiedNode `json:"survivor"`
	Revision       string        `json:"revision"`
	TargetRevision string        `json:"target_revision,omitempty"`
}

// TrustedCertifiedJoin is one accepted statement and the origin evidence the
// trusted loader bound to its pair, survivor and revision.
type TrustedCertifiedJoin struct {
	Join     CertifiedJoin
	Evidence semreg.EvidenceRef
}

// CertifiedJoinEvidence is the origin evidence of one accepted statement. It
// is a function of the pair, the survivor and the revision, so a revocation of
// that statement repeats it and a change of the ordinary section does not.
func CertifiedJoinEvidence(join CertifiedJoin) semreg.EvidenceRef {
	parts := []string{join.A.Bridge, join.A.ID, join.B.Bridge, join.B.ID, join.Survivor.Bridge, join.Survivor.ID, join.Revision}
	sum := sha256.Sum256([]byte(certifiedJoinEvidenceDomain + strings.Join(parts, "\x00")))
	return semreg.EvidenceRef{
		Owner:     "helianthus.gateway.configuration",
		Kind:      "configuration.certified_join",
		Digest:    semreg.Digest("sha256:" + hex.EncodeToString(sum[:])),
		Contract:  semreg.ContractKernelV1,
		Access:    semreg.EvidenceAccessAuthorized,
		Redaction: semreg.RedactionMetadataOnly,
	}
}

// LoadTrustedCertifiedJoins reads the host-local section of a configuration
// root. A missing root or a missing file is an empty section. The ordinary
// decoder does not accept the section; this loader does, and it rejects a
// statement that carries its own origin. A section that does not decode fails
// closed.
func LoadTrustedCertifiedJoins(root string) ([]TrustedCertifiedJoin, error) {
	_, joins, err := loadOperatorRecordsDocument(root)
	if err != nil {
		return nil, err
	}
	if len(joins) == 0 {
		return nil, nil
	}
	out := make([]TrustedCertifiedJoin, len(joins))
	for i, join := range joins {
		out[i] = TrustedCertifiedJoin{Join: join, Evidence: CertifiedJoinEvidence(join)}
	}
	return out, nil
}

func decodeCertifiedJoins(raw any) ([]CertifiedJoin, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, operatorFailure("invalid_record", "certified_joins")
	}
	joins := make([]CertifiedJoin, 0, len(list))
	for i, item := range list {
		field := "certified_joins[" + strconv.Itoa(i) + "]"
		object, ok := item.(map[string]any)
		if !ok {
			return nil, operatorFailure("invalid_record", field)
		}
		action, _ := object["action"].(string)
		shape := map[string]bool{"action": true, "a": true, "b": true, "survivor": true, "revision": true}
		switch action {
		case CertifiedJoinCertify, CertifiedJoinRebind:
		case CertifiedJoinRevoke:
			shape["target_revision"] = true
		default:
			return nil, operatorFailure("invalid_record", field+".action")
		}
		if !hasExactly(object, shape) {
			return nil, operatorFailure("invalid_record", field)
		}
		join := CertifiedJoin{Action: action}
		var err error
		if join.A, err = decodeCertifiedNode(object["a"], field+".a"); err != nil {
			return nil, err
		}
		if join.B, err = decodeCertifiedNode(object["b"], field+".b"); err != nil {
			return nil, err
		}
		if join.Survivor, err = decodeCertifiedNode(object["survivor"], field+".survivor"); err != nil {
			return nil, err
		}
		revision, _ := object["revision"].(string)
		if !canonicalRevision(revision) {
			return nil, operatorFailure("invalid_record", field+".revision")
		}
		join.Revision = revision
		if action == CertifiedJoinRevoke {
			target, _ := object["target_revision"].(string)
			if !canonicalRevision(target) {
				return nil, operatorFailure("invalid_record", field+".target_revision")
			}
			join.TargetRevision = target
		}
		if join.A == join.B {
			return nil, operatorFailure("invalid_record", field+".b")
		}
		if join.Survivor != join.A && join.Survivor != join.B {
			return nil, operatorFailure("invalid_record", field+".survivor")
		}
		joins = append(joins, join)
	}
	return joins, nil
}

func decodeCertifiedNode(raw any, field string) (CertifiedNode, error) {
	object, ok := raw.(map[string]any)
	if !ok || !hasExactly(object, map[string]bool{"bridge": true, "id": true}) {
		return CertifiedNode{}, operatorFailure("invalid_record", field)
	}
	bridge, bridgeOK := object["bridge"].(string)
	id, idOK := object["id"].(string)
	if !bridgeOK || !idOK || semreg.ResourceID(bridge).Validate() != nil || semreg.ResourceID(id).Validate() != nil {
		return CertifiedNode{}, operatorFailure("invalid_record", field)
	}
	return CertifiedNode{Bridge: bridge, ID: id}, nil
}

func canonicalRevision(value string) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == value
}

// withCertifiedJoins returns the ordinary section document with the accepted
// host-local statements appended. An empty section leaves the ordinary bytes
// unchanged.
func withCertifiedJoins(ordinary []byte, joins []CertifiedJoin) ([]byte, error) {
	if len(joins) == 0 {
		return ordinary, nil
	}
	var document operatorRecordDocument
	if err := json.Unmarshal(ordinary, &document); err != nil {
		return nil, err
	}
	document.CertifiedJoins = append([]CertifiedJoin(nil), joins...)
	return json.Marshal(document)
}

// operatorRecordDocument is the on-disk section. The ordinary set omits the
// host-local member, so a response built from OperatorRecordSet cannot carry it.
type operatorRecordDocument struct {
	Format         string           `json:"format"`
	Revision       string           `json:"revision"`
	Records        []OperatorRecord `json:"records"`
	CertifiedJoins []CertifiedJoin  `json:"certified_joins,omitempty"`
}

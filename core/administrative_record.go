package core

import (
	"fmt"

	"github.com/geistesk/dtn7/bundle"
)

// AdministrativeRecordTypeCode specifies the type of an AdministrativeRecord.
// However, currently the Bundle Status Report is the only known type.
type AdministrativeRecordTypeCode uint

const (
	// BundleStatusReportTypeCode is the Bundle Status Report's type code, used in
	// its parent Administrative Record.
	BundleStatusReportTypeCode AdministrativeRecordTypeCode = 1
)

func (artc AdministrativeRecordTypeCode) String() string {
	switch artc {
	case BundleStatusReportTypeCode:
		return "bundle status report"

	default:
		return "unknown"
	}
}

// AdministrativeRecord is a application data unit used for administrative
// records. There is only one kind of Administrative Record definded today: the
// Bundle Status Report.
type AdministrativeRecord struct {
	_struct struct{} `codec:",toarray"`

	TypeCode AdministrativeRecordTypeCode
	Content  interface{}
}

// NewAdministrativeRecord generates a new Administrative Record based on the
// given parameters.
func NewAdministrativeRecord(typeCode AdministrativeRecordTypeCode, content interface{}) AdministrativeRecord {
	return AdministrativeRecord{
		TypeCode: typeCode,
		Content:  content,
	}
}

// ToCanonicalBlock creates a canonical block, containing this administrative
// record. The surrounding bundle _must_ have a set AdministrativeRecordPayload
// bundle processing control flag.
func (ar AdministrativeRecord) ToCanonicalBlock() bundle.CanonicalBlock {
	return bundle.NewCanonicalBlock(bundle.PayloadBlock, 0, 0, ar)
}

func (ar AdministrativeRecord) String() string {
	return fmt.Sprintf("AdministrativeRecord(%s, %v)", ar.TypeCode, ar.Content)
}

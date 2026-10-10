package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The API stores a policy's synthetic-full cadence in hours and accepts
// synthetic_full_cadence_hours or the older synthetic_full_cadence_days. The
// resource sends only the one the configuration sets: sending a value Terraform
// does not know yet, or a stale computed one, would make the API refuse two
// fields that disagree.
func TestBuildBackupPolicyBodySendsOnlyTheCadenceTheConfigurationSets(t *testing.T) {
	base := backupPolicyModel{
		Name: strVal("p"), ScheduleCron: strVal("*/15 * * * *"), RetainDays: types.Int64Value(2),
		SkewPolicy: strVal("flag-and-keep"), COSBucket: strVal("b"), COSRegion: strVal("eu-gb"),
	}
	for _, tc := range []struct {
		name           string
		hours, days    types.Int64
		wantH, wantD   bool
		wantHoursValue int64
		wantDaysValue  int64
	}{
		{"hours only", types.Int64Value(1), types.Int64Unknown(), true, false, 1, 0},
		{"days only", types.Int64Unknown(), types.Int64Value(3), false, true, 0, 3},
		{"neither", types.Int64Null(), types.Int64Null(), false, false, 0, 0},
		{"both", types.Int64Value(48), types.Int64Value(2), true, true, 48, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := base
			plan.SyntheticFullCadenceHours, plan.SyntheticFullCadenceDays = tc.hours, tc.days
			body := buildBackupPolicyBody(plan)
			h, hasH := body["synthetic_full_cadence_hours"]
			d, hasD := body["synthetic_full_cadence_days"]
			if hasH != tc.wantH || (hasH && h != tc.wantHoursValue) {
				t.Errorf("synthetic_full_cadence_hours sent=%v value=%v, want sent=%v value=%v", hasH, h, tc.wantH, tc.wantHoursValue)
			}
			if hasD != tc.wantD || (hasD && d != tc.wantDaysValue) {
				t.Errorf("synthetic_full_cadence_days sent=%v value=%v, want sent=%v value=%v", hasD, d, tc.wantD, tc.wantDaysValue)
			}
		})
	}
}

// A policy set in hours that are not a whole number of days comes back with no
// days key. The state must hold the hours and a null days, not a 0 days that
// the next plan would try to write.
func TestApiToStateReadsTheCadenceInHours(t *testing.T) {
	for _, tc := range []struct {
		body      string
		wantHours int64
		wantDays  types.Int64
	}{
		{`{"synthetic_full_cadence_hours": 1}`, 1, types.Int64Null()},
		{`{"synthetic_full_cadence_hours": 72, "synthetic_full_cadence_days": 3}`, 72, types.Int64Value(3)},
	} {
		var api apiBackupPolicy
		if err := json.Unmarshal([]byte(tc.body), &api); err != nil {
			t.Fatalf("decode %s: %v", tc.body, err)
		}
		var m backupPolicyModel
		apiToState(&api, &m)
		if !m.SyntheticFullCadenceHours.Equal(types.Int64Value(tc.wantHours)) {
			t.Errorf("%s: hours in state = %v, want %d", tc.body, m.SyntheticFullCadenceHours, tc.wantHours)
		}
		if !m.SyntheticFullCadenceDays.Equal(tc.wantDays) {
			t.Errorf("%s: days in state = %v, want %v", tc.body, m.SyntheticFullCadenceDays, tc.wantDays)
		}
	}
}

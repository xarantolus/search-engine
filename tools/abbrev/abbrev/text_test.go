package abbrev

import (
	"reflect"
	"testing"
)

func TestExtractAbbreviations(t *testing.T) {
	tests := []struct {
		text        string
		wantMapping map[string][]string
	}{
		{
			text:        "Technical University\n of Munich (TUM) is a university.\n\nDie Technische Universität München (TUM) ist eine der Universitäten.",
			wantMapping: map[string][]string{"tum": {"Technical University of Munich", "Technische Universität München"}},
		},
		{
			text:        "the united states of america (usa) is a country.",
			wantMapping: map[string][]string{"usa": {"united states of america"}},
		},
		{
			text:        "Thermal Vacuum Chamber (TVAC) is used for testing.",
			wantMapping: map[string][]string{"tvac": {"Thermal Vacuum Chamber"}},
		},
		{
			text:        "An embedded MultiMediaCard (eMMC) is a small storage device",
			wantMapping: map[string][]string{"emmc": {"embedded MultiMediaCard"}},
		},
		{
			text:        "      FLG(15) = 16                                                              \n      FLG(16) = 7                                                               \n      FLG(17) = 92                                                              ",
			wantMapping: map[string][]string{},
		},
		{
			text:        "var aa=C(r);aa.setAttribute(\"type\",q);var Z=X.appendChild(aa);",
			wantMapping: map[string][]string{},
		},
		{
			text:        "(Abc Bdc Cdc (ABC))",
			wantMapping: map[string][]string{"abc": {"Abc Bdc Cdc"}},
		},
		{
			text:        "\"Abc Bdc Cdc (ABC)\"",
			wantMapping: map[string][]string{"abc": {"Abc Bdc Cdc"}},
		},
		{
			text:        "The Ground-Segment (GS) is",
			wantMapping: map[string][]string{"gs": {"Ground-Segment"}},
		},
		{
			text:        "The Ground Sta-\ntion (GS) is a part of the system.",
			wantMapping: map[string][]string{"gs": {"Ground Station"}},
		},
		{
			text:        "The Ground Sta-  \n\t tion (GS) is a part of the system.",
			wantMapping: map[string][]string{"gs": {"Ground Station"}},
		},
		{
			text:        "Satellit beim Deutschen Zentrum für Luft- und Raumfahrt (DLR)",
			wantMapping: map[string][]string{"dlr": {"Deutschen Zentrum für Luft- und Raumfahrt"}},
		},
		{
			text:        "The Operations (OPS) team",
			wantMapping: map[string][]string{"ops": {"Operations"}},
		},
		{
			text:        "The Communications (COM) team",
			wantMapping: map[string][]string{"com": {"Communications"}},
		},
		{
			text:        "via Inter-Integrated Circuit (I2C)",
			wantMapping: map[string][]string{"i2c": {"Inter-Integrated Circuit"}},
		},
	}

	for _, tt := range tests {
		t.Run(t.Name(), func(t *testing.T) {
			if gotMapping := ExtractAbbreviations(tt.text, 100); !reflect.DeepEqual(gotMapping, tt.wantMapping) {
				t.Errorf("ExtractAbbreviations(%#v) = %#v, want %#v", tt.text, gotMapping, tt.wantMapping)
			}
		})
	}
}

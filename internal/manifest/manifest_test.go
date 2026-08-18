package manifest

import "testing"

func TestValidateSDKBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   Manifest
		wantErr bool
	}{
		{name: "sdk v3", value: Manifest{ID: "com.example", Version: "1.0.0", SDK: 3}},
		{name: "sdk v2", value: Manifest{ID: "com.example", Version: "1.0.0", SDK: 2}, wantErr: true},
		{name: "duplicate driver", value: Manifest{ID: "com.example", Version: "1.0.0", SDK: 3, Drivers: []DriverManifest{{ID: "x"}, {ID: "x"}}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.value.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

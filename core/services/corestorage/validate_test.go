package corestorage

import "testing"

func TestValidateCoreStorageConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"path ok when absolute and confirmed", Config{Backend: "path", Path: "/mnt/shared", PathConfirmed: true}, false},
		{"path rejected when not confirmed", Config{Backend: "path", Path: "/mnt/shared", PathConfirmed: false}, true},
		{"path rejected when relative", Config{Backend: "path", Path: "relative/dir", PathConfirmed: true}, true},
		{"path rejected when empty", Config{Backend: "path", Path: "", PathConfirmed: true}, true},
		{"local alias treated like path", Config{Backend: "local", Path: "/mnt/shared", PathConfirmed: true}, false},
		{"s3 ok with bucket+creds", Config{Backend: "s3", S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s"}, false},
		{"s3 rejected without bucket", Config{Backend: "s3", S3AccessKey: "k", S3SecretKey: "s"}, true},
		{"s3 rejected without access key", Config{Backend: "s3", S3Bucket: "b", S3SecretKey: "s"}, true},
		{"s3 rejected without secret", Config{Backend: "s3", S3Bucket: "b", S3AccessKey: "k"}, true},
		{"unknown backend rejected", Config{Backend: "ftp"}, true},
		{"empty backend rejected (unconfigured)", Config{Backend: ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.cfg)
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate(%+v) err = %v, wantErr %v", c.cfg, err, c.wantErr)
			}
		})
	}
}

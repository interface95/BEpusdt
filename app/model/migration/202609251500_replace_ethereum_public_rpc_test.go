package migration

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestRunReplacesOnlyLegacyEthereumPublicRPC(t *testing.T) {
	const publicNode = "https://ethereum-rpc.publicnode.com"
	for _, tc := range []struct {
		name string
		old  string
		want string
	}{
		{"legacy", "https://ethereum-public.nodies.app/", publicNode},
		{"legacy_without_slash", "https://ethereum-public.nodies.app", publicNode},
		{"publicnode", publicNode, publicNode},
		{"custom", "https://rpc.example.com/private", "https://rpc.example.com/private"},
		{"custom_nodies_path", "https://ethereum-public.nodies.app/private", "https://ethereum-public.nodies.app/private"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			if err := db.Exec("CREATE TABLE bep_conf (k VARCHAR(32) PRIMARY KEY, v VARCHAR(512) NOT NULL)").Error; err != nil {
				t.Fatal(err)
			}
			const legacy = "https://ethereum-public.nodies.app/"
			if err := db.Exec("INSERT INTO bep_conf (k, v) VALUES (?, ?), (?, ?)",
				"rpc_endpoint_ethereum", tc.old, "rpc_endpoint_bsc", legacy).Error; err != nil {
				t.Fatal(err)
			}
			if err := Run(db, nil); err != nil {
				t.Fatal(err)
			}
			var rows []struct{ K, V string }
			if err := db.Table("bep_conf").Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				want := tc.want
				if row.K == "rpc_endpoint_bsc" {
					want = legacy
				}
				if row.V != want {
					t.Fatalf("%s = %q, want %q", row.K, row.V, want)
				}
			}
			if err := db.Exec("UPDATE bep_conf SET v = ? WHERE k = ?", legacy, "rpc_endpoint_ethereum").Error; err != nil {
				t.Fatal(err)
			}
			if err := Run(db, nil); err != nil {
				t.Fatal(err)
			}
			var value string
			if err := db.Table("bep_conf").Select("v").Where("k = ?", "rpc_endpoint_ethereum").Scan(&value).Error; err != nil {
				t.Fatal(err)
			}
			if value != legacy {
				t.Fatal("migration ran again after the operator changed the endpoint")
			}
		})
	}
}

func TestRunWithNoExistingEthereumRPC(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "fresh.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := Run(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE bep_conf (k VARCHAR(32) PRIMARY KEY, v VARCHAR(512) NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Run(db, nil); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("bep_conf").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("migration must not seed an absent configuration")
	}
}

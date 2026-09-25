package migration

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func m202609251500ReplaceEthereumPublicRPC() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "202609251500_replace_ethereum_public_rpc",
		Migrate: func(tx *gorm.DB) error {
			if !tx.Migrator().HasTable("bep_conf") {
				return nil
			}
			// Replace only the retired public default, not operator-managed endpoints.
			return tx.Table("bep_conf").
				Where("k = ? AND v IN ?", "rpc_endpoint_ethereum", []string{
					"https://ethereum-public.nodies.app/",
					"https://ethereum-public.nodies.app",
				}).
				Update("v", "https://ethereum-rpc.publicnode.com").Error
		},
	}
}

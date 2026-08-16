package config

import (
	"strings"
	"testing"
)

func TestLoadUsesSeparateAdminAndSyncDefaults(t *testing.T) {
	t.Setenv(databaseURLSetting, "postgres://vive:password@postgres/vive")
	t.Setenv(adminListenAddressSetting, "")
	t.Setenv(syncListenAddressSetting, "")
	t.Setenv(logLevelSetting, "")
	t.Setenv(shutdownTimeoutSetting, "")
	t.Setenv(signupModeSetting, "")

	settings, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if settings.AdminListenAddress != ":8080" {
		t.Fatalf("admin address = %q, want :8080", settings.AdminListenAddress)
	}
	if settings.SyncListenAddress != ":8281" {
		t.Fatalf("sync address = %q, want :8281", settings.SyncListenAddress)
	}
}

func TestLoadRejectsOneAddressForBothSurfaces(t *testing.T) {
	t.Setenv(databaseURLSetting, "postgres://vive:password@postgres/vive")
	t.Setenv(adminListenAddressSetting, ":9000")
	t.Setenv(syncListenAddressSetting, ":9000")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must use different addresses") {
		t.Fatalf("Load error = %v, want different-address validation", err)
	}
}

package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestScaleLoadConfig_ValidateAPIRateConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		config      ScaleLoadConfig
		wantError   bool
		errorString string
	}{
		{
			name: "valid static rate only",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRateStatic: int32Ptr(5000),
					},
				},
			},
			wantError: false,
		},
		{
			name: "valid per-node rate only",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRatePerNode: int32Ptr(100),
					},
				},
			},
			wantError: false,
		},
		{
			name: "valid deprecated field only",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRate: int32Ptr(20),
					},
				},
			},
			wantError: false,
		},
		{
			name: "valid no rate fields (defaults)",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{},
				},
			},
			wantError: false,
		},
		{
			name: "invalid both static and per-node",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRateStatic:  int32Ptr(5000),
						APICallRatePerNode: int32Ptr(100),
					},
				},
			},
			wantError:   true,
			errorString: "only one API rate limiting approach can be specified",
		},
		{
			name: "invalid all three fields",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRate:        int32Ptr(20),
						APICallRateStatic:  int32Ptr(5000),
						APICallRatePerNode: int32Ptr(100),
					},
				},
			},
			wantError:   true,
			errorString: "only one API rate limiting approach can be specified",
		},
		{
			name: "invalid static and deprecated",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRate:       int32Ptr(20),
						APICallRateStatic: int32Ptr(5000),
					},
				},
			},
			wantError:   true,
			errorString: "only one API rate limiting approach can be specified",
		},
		{
			name: "invalid per-node and deprecated",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRate:        int32Ptr(20),
						APICallRatePerNode: int32Ptr(100),
					},
				},
			},
			wantError:   true,
			errorString: "only one API rate limiting approach can be specified",
		},
		{
			name: "invalid negative static rate",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRateStatic: int32Ptr(-1000),
					},
				},
			},
			wantError:   true,
			errorString: "apiCallRateStatic must be positive",
		},
		{
			name: "invalid zero per-node rate",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRatePerNode: int32Ptr(0),
					},
				},
			},
			wantError:   true,
			errorString: "apiCallRatePerNode must be positive",
		},
		{
			name: "invalid negative deprecated rate",
			config: ScaleLoadConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: ScaleLoadConfigSpec{
					LoadProfile: LoadProfile{
						APICallRate: int32Ptr(-5),
					},
				},
			},
			wantError:   true,
			errorString: "apiCallRate must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validateAPIRateConfiguration()

			if tt.wantError {
				if err == nil {
					t.Errorf("Expected error but got none")
					return
				}
				if tt.errorString != "" && !contains(err.Error(), tt.errorString) {
					t.Errorf("Expected error containing '%s', got '%s'", tt.errorString, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error but got: %v", err)
				}
			}
		})
	}
}

func TestScaleLoadConfig_ValidateCreate(t *testing.T) {
	config := ScaleLoadConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: ScaleLoadConfigSpec{
			LoadProfile: LoadProfile{
				APICallRateStatic:  int32Ptr(5000),
				APICallRatePerNode: int32Ptr(100), // This should cause validation to fail
			},
		},
	}

	err := config.ValidateCreate()
	if err == nil {
		t.Error("Expected ValidateCreate to fail with both rate fields set")
	}
}

func TestScaleLoadConfig_ValidateUpdate(t *testing.T) {
	config := ScaleLoadConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: ScaleLoadConfigSpec{
			LoadProfile: LoadProfile{
				APICallRateStatic: int32Ptr(5000),
			},
		},
	}

	oldConfig := ScaleLoadConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: ScaleLoadConfigSpec{
			LoadProfile: LoadProfile{
				APICallRatePerNode: int32Ptr(100),
			},
		},
	}

	err := config.ValidateUpdate(&oldConfig)
	if err != nil {
		t.Errorf("Expected ValidateUpdate to pass with valid config, got: %v", err)
	}
}

// Helper functions
func int32Ptr(i int32) *int32 {
	return &i
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			containsSubstring(s, substr))))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

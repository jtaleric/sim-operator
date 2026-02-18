# Testing Documentation

This document describes the testing setup and GitHub Actions workflows for the sim-operator project.

## Testing Structure

### Unit Tests
- **Location**: `api/v1/*_test.go`, `internal/controllers/*_test.go`
- **Focus**: API validation, controller logic, resource management
- **Run locally**: `make test` or `make test-all`

### API Validation Tests
- **Location**: `api/v1/scaleloadconfig_validation_test.go`
- **Focus**: Webhook validation of API rate limiting configurations
- **Run locally**: `make test-api-validation`

### Configuration Validation
- **Location**: `test/validation/`
- **Files**:
  - `valid_configs.yaml`: Valid configurations for testing
  - `invalid_configs.yaml`: Invalid configurations that should be rejected
  - `README.md`: Documentation of validation rules

## GitHub Actions Workflows

### 1. PR Tests (`.github/workflows/pr-tests.yaml`)

Comprehensive testing on every pull request:

- **Triggers**: PRs to main/master, changes to Go files, configs, etc.
- **Jobs**:
  - **Test**: Unit tests, coverage analysis (requires >50%)
  - **Build**: Generate manifests, build binary and Docker image
  - **Lint**: golangci-lint for code quality
  - **Validate Configs**: YAML syntax and CRD schema validation
  - **Security Scan**: Gosec security analysis

### 2. API Validation (`.github/workflows/api-validation.yaml`)

Focused testing for API validation:

- **Triggers**: Changes to `api/`, `test/validation/`, or sample configs
- **Jobs**:
  - **API Validation Tests**: Run webhook validation unit tests
  - **Sample Config Validation**: Verify samples follow validation rules
  - **Test Structure Validation**: Ensure test files are properly structured
  - **Validation Report**: Generate and comment on PRs with results

## Local Testing Commands

```bash
# Run all tests
make test-all

# Run only API validation tests
make test-api-validation

# Run comprehensive validation tests
make test-validation-comprehensive

# Validate YAML syntax (requires yq)
make validate-configs

# Run linter
make lint

# Fix linting issues
make lint-fix
```

## Test Coverage

Current test coverage focuses on:
- ✅ API validation logic (dual rate limiting)
- ✅ Webhook validation scenarios
- ✅ Configuration syntax validation
- ⚠️ Controller logic (needs expansion)
- ⚠️ Resource management (needs expansion)

## Adding New Tests

### For API Changes
1. Add unit tests in `api/v1/*_test.go`
2. Add validation scenarios to `test/validation/invalid_configs.yaml` if needed
3. Update sample configs in `config/samples/`

### For Controller Changes
1. Add tests in `internal/controllers/*_test.go`
2. Mock external dependencies (Kubernetes API calls)
3. Test different scenarios and edge cases

## Continuous Integration

The CI pipeline ensures:
- Code quality (linting, formatting)
- Test coverage (minimum 50%)
- Security scanning
- Proper manifest generation
- Configuration validation
- Docker image building

## Configuration Validation Rules

### API Rate Limiting
- Only one rate field allowed: `apiCallRateStatic` OR `apiCallRatePerNode` OR `apiCallRate` (deprecated)
- All rate values must be positive integers
- Clear error messages guide users to correct configuration

### Sample Configs
- Must pass YAML syntax validation
- Must comply with CRD schema
- Should demonstrate different use cases
- Are automatically validated in CI

## Troubleshooting

### Test Failures
- Check test output for specific failures
- Ensure dependencies are installed (`go mod download`)
- Verify Go version compatibility

### Validation Failures
- Check YAML syntax with `yq eval`
- Verify against CRD schema
- Review validation error messages

### CI Failures
- Check workflow logs in GitHub Actions
- Verify all required checks pass
- Address linting or security issues
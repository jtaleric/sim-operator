# Validation Test Cases

This directory contains test cases for webhook validation of ScaleLoadConfig API rate limiting.

## Files

- **`valid_configs.yaml`**: Valid configurations that should pass webhook validation
- **`invalid_configs.yaml`**: Invalid configurations that should be rejected by webhook validation

## API Rate Limiting Validation Rules

### ✅ Valid Configurations

1. **Static rate only**: `apiCallRateStatic: 5000`
2. **Per-node rate only**: `apiCallRatePerNode: 100`
3. **Deprecated field only**: `apiCallRate: 20` (backward compatibility)
4. **No rate fields**: Uses default (20 calls/min/node)

### ❌ Invalid Configurations

1. **Multiple fields set**: Any combination of 2+ rate fields
2. **Negative values**: Any negative rate values
3. **Zero values**: Any zero rate values

## Testing with kubectl

To test validation (requires webhook deployed):

```bash
# Valid configs should succeed
kubectl apply -f test/validation/valid_configs.yaml

# Invalid configs should fail with validation errors
kubectl apply -f test/validation/invalid_configs.yaml
```

## Example Error Messages

```
error validating data: ValidationError(ScaleLoadConfig.spec.loadProfile): 
only one API rate limiting approach can be specified, found: [apiCallRateStatic apiCallRatePerNode]. 
Choose either 'apiCallRateStatic' for fixed total rate or 'apiCallRatePerNode' for node-scaling rate. 
Note: 'apiCallRate' is deprecated, use 'apiCallRatePerNode' instead
```

## Rate Limiting Approaches

### Static Rate (`apiCallRateStatic`)
- Fixed total API calls per minute regardless of cluster size
- Best for: Testing specific API server limits, predictable load testing
- Example: `apiCallRateStatic: 5000` = always 5000 calls/minute (83/second)

### Per-Node Rate (`apiCallRatePerNode`)
- API calls scale linearly with node count
- Best for: Realistic workload simulation, cluster scaling tests
- Example: `apiCallRatePerNode: 200` with 500 nodes = 100,000 calls/minute (1667/second)

### Deprecated Rate (`apiCallRate`)
- Treated as per-node rate for backward compatibility
- Should migrate to `apiCallRatePerNode` for clarity
## Checklist

- [ ] go vet ./...
- [ ] go test ./... -race -timeout 60s
- [ ] make ci
- [ ] kubectl kustomize deploy/kustomize/overlays/local
- [ ] No direct pushes to main

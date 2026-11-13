default: testacc

# Run acceptance tests
.PHONY: testacc
testacc:
	TF_ACC=1 go test ./... -count=1 -v $(TESTARGS) -timeout 120m

.PHONY:
test:
	go test ./... -count=1 -v $(TESTARGS) -timeout 120m

.PHONY: install
install:
	go install

.PHONY: plan
plan: install
	terraform -chdir=./examples/end-to-end/ plan

.PHONY: apply
apply: install
	terraform -chdir=./examples/end-to-end/ apply

# Print the changelog entries that are not in a release yet
.PHONY: changelog-preview
changelog-preview:
	./scripts/changelog.sh preview

# Print the release notes for the commit that is checked out. The release workflow uses this.
.PHONY: changelog-notes
changelog-notes:
	./scripts/changelog.sh notes

# Check the format of the files in .changelog
.PHONY: changelog-check
changelog-check:
	./scripts/changelog.sh check

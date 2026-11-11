default: testacc

# Run acceptance tests
.PHONY: testacc
testacc:
	TF_ACC=1 go test ./... -v $(TESTARGS) -timeout 120m

# Run the acceptance tests that need a multi-broker cluster. These tests start
# their own three-broker cluster and are skipped without KAFKA_TEST_BROKERS=3.
.PHONY: testacc-multibroker
testacc-multibroker:
	KAFKA_TEST_BROKERS=3 TF_ACC=1 go test ./internal/provider -run MultiBroker -v $(TESTARGS) -timeout 30m

.PHONY:
test:
	go test ./... -v $(TESTARGS) -timeout 120m

.PHONY: install
install:
	go install

.PHONY: plan
plan: install
	terraform -chdir=./examples/end-to-end/ plan

.PHONY: apply
apply: install
	terraform -chdir=./examples/end-to-end/ apply

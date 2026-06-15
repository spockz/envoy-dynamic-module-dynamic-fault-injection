.PHONY: help build test integration-test clean

CYAN := \033[36m
GREEN := \033[32m
YELLOW := \033[33m
RESET := \033[0m
BOLD := \033[1m

define print_task
	printf "$(BOLD)$(CYAN)[TASK]$(RESET) $(BOLD)%s$(RESET)\n" "$(1)"
endef
define print_success
	printf "  $(GREEN)✓$(RESET) %s\n" "$(1)"
endef

## help: Show this help info.
help:
	@echo "Envoy Latency and Fault Distribution Simulation - Dynamic Module\n"
	@echo "Usage:\n  make \033[36m<Target>\033[0m \n\nTargets:"
	@awk 'BEGIN {FS = ":.*##"; printf ""} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

build: ## Build the Go dynamic module shared library.
	@$(call print_task,Building Go dynamic module)
	CGO_ENABLED=1 go build -buildmode=c-shared -o liblatency_fault_module.so .
	@$(call print_success,Built liblatency_fault_module.so)

test: ## Run unit tests.
	@$(call print_task,Running unit tests)
	go test -v -race ./...
	@$(call print_success,Unit tests passed)

integration-test: build ## Run integration tests with Envoy.
	@$(call print_task,Copying module for integration tests)
	cp liblatency_fault_module.so integration/liblatency_fault_module.so
	@$(call print_success,Module copied to integration/)
	@$(call print_task,Running integration tests)
	cd integration && go test -v -timeout 300s ./...
	@$(call print_success,Integration tests passed)

clean: ## Remove build artifacts.
	@$(call print_task,Cleaning build artifacts)
	rm -f liblatency_fault_module.so
	rm -f integration/liblatency_fault_module.so
	@$(call print_success,Clean complete)

tidy: ## Tidy Go modules.
	@$(call print_task,Tidying Go modules)
	go mod tidy
	cd integration && go mod tidy
	@$(call print_success,Modules tidied)

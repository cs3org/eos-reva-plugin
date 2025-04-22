# force makefile to use bash instead of sh.
SHELL := /usr/bin/env bash

################################################################################
# Toolchain
################################################################################

TOOLCHAIN		?= $(CURDIR)/toolchain
GOLANGCI_LINT	?= $(TOOLCHAIN)/golangci-lint

.PHONY: toolchain
toolchain: $(GOLANGCI_LINT) $(CALENS)

$(GOLANGCI_LINT):
	@mkdir -p $(@D)
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | BINDIR=$(@D) sh -s v2.1.2

.PHONY: lint
lint: $(GOLANGCI_LINT)
	@$(GOLANGCI_LINT) run || (echo "Tip: many lint errors can be automatically fixed with \"make lint-fix\""; exit 1)

.PHONY: lint-fix
lint-fix: $(GOLANGCI_LINT)
	gofmt -w .
	$(GOLANGCI_LINT) run --fix
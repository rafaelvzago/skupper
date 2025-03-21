ROOT_PATH := $(shell pwd)
EXTRA_VARS := --extra-vars "@$(ROOT_PATH)/vars.yml"
COLLECTION_PATH := $(ROOT_PATH)/e2e/collections/ansible_collections/e2e/tests

# E2E Test directories
E2E_TEST_DIRS := $(sort $(wildcard $(ROOT_PATH)/e2e/scenarios/*))

# Creating a python virtual environment
create-venv:
	@if [ "$(FORCE)" = "true" ] || [ ! -d "/tmp/e2e-venv" ]; then \
		bash -c "deactivate 2>/dev/null || true"; \
		echo "Removing old environment if it exists..."; \
		rm -rf /tmp/e2e-venv; \
		echo "Creating virtual environment..."; \
		cd /tmp && python3 -m venv e2e-venv; \
		if [ -d "/tmp/e2e-venv" ]; then \
			echo "Virtual environment created successfully at /tmp/e2e-venv"; \
			source /tmp/e2e-venv/bin/activate;  \
			pip install --upgrade pip; \
			pip install -r $(ROOT_PATH)/requirements.txt; \
			ANSIBLE_CONFIG=$(ROOT_PATH)/e2e/ansible.cfg ansible-galaxy collection install -r $(ROOT_PATH)/e2e/collections/ansible_collections/requirements.yml --force; \
		else \
			echo "Failed to create virtual environment at /tmp/e2e-venv"; \
			exit 1; \
		fi; \
	else \
		echo "Using the existing virtual environment at /tmp/e2e-venv"; \
		if [ ! -d "/tmp/e2e-venv" ]; then \
			echo "Virtual environment does not exist at /tmp/e2e-venv"; \
			echo "Run 'make create-venv FORCE=true' to create it"; \
			exit 1; \
		fi; \
	fi

# Run tests for a specific role given as parameter
test-role: create-venv
	# Returning if ROLE is not set
	@if [ -z "$(ROLE)" ]; then \
		echo "ROLE is not set. Please provide a ROLE to run the tests."; \
		exit 1; \
	fi
	@echo "Running the tests for the role: $(ROLE)"
	@cd $(COLLECTION_PATH) && \
	source /tmp/e2e-venv/bin/activate && \
	ANSIBLE_CONFIG=$(ROOT_PATH)/e2e/ansible.cfg ansible-playbook roles/$(ROLE)/tests/test_playbook.yml -i roles/$(ROLE)/tests/inventory $(EXTRA_VARS)

# Run a specific test (use relative path from e2e/ directory)
test: FORCE=false
test: create-venv
	@if [ -z "$(TEST)" ]; then \
		echo "TEST is not set. Please provide a TEST to run the tests."; \
		exit 1; \
	fi

	@export TEST_PREFIX=$$(cat /dev/urandom | tr -dc 'a-z0-9' | fold -w 5 | head -n 1); \
	echo "Running the test for directory: $(TEST) with prefix: $$TEST_PREFIX"; \
	source /tmp/e2e-venv/bin/activate && \
	ANSIBLE_CONFIG=$(ROOT_PATH)/e2e/ansible.cfg ansible-playbook $(ROOT_PATH)/e2e/scenarios/$(TEST)/test.yml -i $(ROOT_PATH)/e2e/scenarios/$(TEST)/inventory $(EXTRA_VARS) -e namespace_prefix=$$TEST_PREFIX

# Run all tests in sequence
e2e-tests:
	@echo "Running all tests in sequence..."
	@for test_dir in $(E2E_TEST_DIRS); do \
		test_name=$$(basename $$test_dir); \
		echo "\n=== Running test: $$test_name ==="; \
		$(MAKE) test TEST="$$test_name" || exit 1; \
	done
	@echo -e "\n=== All tests completed successfully ==="

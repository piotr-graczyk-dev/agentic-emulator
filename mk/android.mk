# Android backend build steps (included by the top-level Makefile via -include mk/*.mk).
#
#   make android-agent   rebuild internal/android/assets/agemu-agent.jar from android-agent/src
#                        (needs a JDK + Android SDK; the jar is committed, so plain `go build`
#                        never needs them)
#   make android-build   bin/android/{agemu,agemud,agemu-bench} (iOS backend compiled out)
#   make android-test    vet + unit tests for the Android backend
#   make android-e2e     scripts/e2e-android.sh against a real agemu-* emulator
#   make android-bench   benchmark table (standard tools vs agemu) on the same emulator

.PHONY: android-agent android-build android-test android-e2e android-bench

ANDROID_SDK_DIR    ?= $(or $(ANDROID_HOME),$(ANDROID_SDK_ROOT),$(if $(filter Darwin,$(shell uname -s)),$(HOME)/Library/Android/sdk,$(HOME)/Android/Sdk))
ANDROID_AGENT_JAR  := internal/android/assets/agemu-agent.jar
ANDROID_AGENT_SRC  := $(shell find android-agent/src -name '*.java' 2>/dev/null)
ANDROID_BIN        := bin/android
ANDROID_DEVICE     ?= android:agemu-pixel

build-extra: android-agent

android-agent: $(ANDROID_AGENT_JAR)

$(ANDROID_AGENT_JAR): $(ANDROID_AGENT_SRC) android-agent/build.sh
	@if command -v javac >/dev/null 2>&1 && [ -d "$(ANDROID_SDK_DIR)/build-tools" ]; then \
		ANDROID_HOME="$(ANDROID_SDK_DIR)" android-agent/build.sh; \
	else \
		echo "android-agent: no JDK/Android SDK, keeping the committed $@"; \
	fi

android-build: android-agent
	go build -o $(ANDROID_BIN)/agemu ./cmd/agemu
	go build -tags noios -o $(ANDROID_BIN)/agemud ./cmd/agemud
	go build -tags noios -o $(ANDROID_BIN)/agemu-bench ./cmd/agemu-bench

android-test:
	go vet -tags noios ./internal/android/... ./cmd/...
	go test -tags noios ./internal/android/... ./internal/daemon/... ./cmd/...

android-e2e: android-build
	scripts/e2e-android.sh

android-bench: android-build
	$(ANDROID_BIN)/agemu-bench --platform android --device $(ANDROID_DEVICE) --agemu $(ANDROID_BIN)/agemu --n 20 --out bench/out

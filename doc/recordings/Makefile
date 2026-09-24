SHELL := /bin/bash
.ONESHELL:

VHS := VHS_NO_SANDBOX=true go run github.com/charmbracelet/vhs@v0.11.0
GIFSICLE_FLAGS := --optimize=3 --careful --lossy=20 --colors 128
RPORT ?= 8443
RECORDINGS := analyse
RAW_TRANSCRIPTS := $(addsuffix .transcript,$(RECORDINGS))
REQUIRED_COMMANDS := ffmpeg gifsicle script ttyd
export RHOST
export RPORT

define NORMALISE_TRANSCRIPT
LC_ALL=C sed \
	-e $$'s|\033\\[[0-?]*[ -/]*[@-~]||g' \
	-e $$'s|\r$$||' \
	-e $$'s|.*\r||' \
	-e '/^Script started on /d' \
	-e '/^Script done on /d' \
	-e '/^logout$$/d'
endef

.INTERMEDIATE: tturl
.PHONY: $(RECORDINGS) clean FORCE prerequisites validate

$(RECORDINGS): prerequisites tturl
	set -eu
	transcript="$@.transcript"
	trap 'rm -f "$$transcript"' EXIT
	case "$$RHOST" in \
		''|*[!A-Za-z0-9.-]*) \
			echo "RHOST must be an IP address or DNS name" >&2; exit 1 ;; \
	esac
	case "$$RPORT" in \
		''|*[!0-9]*|??????*) \
			echo "RPORT must be a TCP port" >&2; exit 1 ;; \
	esac
	if ((10#$$RPORT < 1 || 10#$$RPORT > 65535)); then \
		echo "RPORT must be a TCP port" >&2; exit 1
	fi
	sed -e "s|{{RHOST}}|$$RHOST|g" -e "s|{{RPORT}}|$$RPORT|g" \
		"$@.tape" | \
		TRANSCRIPT="$$transcript" PATH="$(CURDIR):$$PATH" $(VHS)
	gifsicle $(GIFSICLE_FLAGS) --batch "$@.gif"
	$(NORMALISE_TRANSCRIPT) "$$transcript" >"$@.txt"

tturl: FORCE
	go build -o $@ ../../cmd/tturl

FORCE:

clean:
	rm -f tturl $(RAW_TRANSCRIPTS)

prerequisites:
	@for command in $(REQUIRED_COMMANDS); do
		command -v "$$command" >/dev/null || \
			{ echo "$$command is required" >&2; exit 1; }
	done

validate:
	$(VHS) validate $(addsuffix .tape,$(RECORDINGS))

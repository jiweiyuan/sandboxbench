# Build the paper. `make` produces main.pdf.
#
# The results table is generated from a SandboxBench run, never typed by hand:
#   make table RESULT=results/<run>.json       (path relative to the repo root)

TEX      := xelatex
# Paths in RESULT are relative to the repository root, not to paper/.
RESULT   ?= $(shell cd .. && ls -t results/*.json 2>/dev/null | head -1)
SECTIONS := $(wildcard sections/*.tex)

.PHONY: all clean table

all: main.pdf

main.pdf: main.tex $(SECTIONS) refs.bib
	$(TEX) -halt-on-error main.tex
	-bibtex main
	$(TEX) -halt-on-error main.tex
	$(TEX) -halt-on-error main.tex

# Regenerate the results table from the newest run, or from RESULT=<file>.
# Always regenerates; the committed file is a placeholder, not a build output.
table:
	@if [ -z "$(RESULT)" ]; then \
	  echo "no result file found; run 'sandboxbench run' first, or pass RESULT=<file>"; \
	  exit 1; \
	fi
	@echo "generating from $(RESULT)"
	go run -C .. ./cmd/sandboxbench report --in "$(RESULT)" --format latex \
	  --out paper/figures/results-table.tex

clean:
	rm -f main.pdf main.aux main.bbl main.blg main.log main.out main.toc

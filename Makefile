# Makefile — convenience targets for wakil development.
#
# `make clean` removes local temp/cache directories and the stale compiled
# binary. These are all gitignored — this is working-directory hygiene, not
# source cleanup. The wakil binary is NOT removed (it may be the active
# development build).

.PHONY: clean

clean:
	rm -rf .tmp-test/ .tmp-gocache/ .tmp-gotest/ .tmp-gobuild/ .gocache/ .gotmp/ __pycache__/
	chmod -R u+w .tmp/ 2>/dev/null; rm -rf .tmp/

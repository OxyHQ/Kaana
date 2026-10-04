# Private Jev card build-context correction

Kaana155 added a Dockerfile copy of configs/provider-rates-jev-scoped.json, but the deny-all .dockerignore excluded it. Release37233604774 failed at that copy before a new image was published. The fix admits only that exact reviewed file; the xAI file, Dockerfile, getter, bindings and runtime code are unchanged.

The retained RED/GREEN context census uses Docker’s github.com/moby/patternmatcher v0.6.0 and ignorefile parser. RED omits the required private card; GREEN admits all four exact config inputs. The full tracked context changes143→144 files, with exactly the private card added. Example/unreviewed config, test helper and node_modules probes remain excluded. No local Docker image or AWS operation ran. Canonical CI/release build must verify the final image.

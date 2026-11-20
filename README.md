# autobot-connector

Lend an idle GPU machine to Flare's automated triage, so changes get tested on hardware the project
does not have.

A small **runner** on the donor's machine starts a fresh, offline container for each test job and
destroys it afterwards; a **link** connects out to the triage hub and fetches the job's inputs from
public sources, verified by digest. The donor's own coding agent sets it up from a brief.

The connector runs on dedicated DGX Spark and four-card RTX PRO 6000 Blackwell Max-Q test machines.

- [Design](docs/DESIGN.md)
- [Runner acceptance](docs/STAGE1.md)
- [Link and hub acceptance](docs/STAGE2.md)
- [Flare donor deployment and hub migration](docs/STAGE3.md)

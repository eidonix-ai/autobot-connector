# autobot-connector

Lend an idle GPU machine to Flare's automated triage, so changes get tested on hardware the project
does not have.

A small **runner** on the donor's machine starts a fresh, offline container for each test job and
destroys it afterwards; a **link** connects out to the triage hub and fetches the job's inputs from
public sources, verified by digest. The donor's own coding agent sets it up from a brief.

Status: design and first prototype. Not ready for donors yet.

- [Design](docs/DESIGN.md)

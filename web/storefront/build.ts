import { cpSync, rmSync } from "node:fs";

// The event page ships no script of ours: the build is just the stylesheet.
rmSync("dist", { recursive: true, force: true });
cpSync("public", "dist", { recursive: true });

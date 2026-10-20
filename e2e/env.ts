// Where the e2e stack lives, shared by start.ts and the specs that reach past the API
// (staff grants run the built binary; expiry edits a row as the owner).
import { tmpdir } from "node:os";
import { join } from "node:path";

export const pg = process.env.E2E_PG ?? "localhost:54329";
export const DB = "ticketslinger_e2e";
export const url = (role: string, db: string) =>
  `postgres://${role}:${role}@${pg}/${db}?sslmode=disable`;
export const bin = join(tmpdir(), "ticketslinger-e2e");
export const ownerUrl = url("ts_owner", DB);

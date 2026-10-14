import createClient from "openapi-fetch";
import type { paths } from "./schema.gen";

export type ApiError = { error: { code: string; message: string } };

export function createApi(baseUrl: string, fetchImpl: (req: Request) => Promise<Response> = fetch) {
  return createClient<paths>({ baseUrl, fetch: fetchImpl });
}

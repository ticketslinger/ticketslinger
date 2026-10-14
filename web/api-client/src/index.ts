import createClient from "openapi-fetch";
import type { components, paths } from "./schema.gen";

export type { components, paths } from "./schema.gen";

export type ApiError = components["schemas"]["Error"];

export function createApi(baseUrl: string, fetchImpl: (req: Request) => Promise<Response> = fetch) {
  return createClient<paths>({ baseUrl, fetch: fetchImpl });
}

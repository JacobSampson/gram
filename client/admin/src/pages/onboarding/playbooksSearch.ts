import { z } from "zod";

const searchSchema = z.object({
  // An organization id. Set from an organization's Overview page, it scopes
  // the Playbooks table to that organization's own playbooks beside the
  // shared ones, and a new playbook belongs to it.
  organization: z.string().optional().catch(undefined),
});

export type PlaybooksSearch = z.infer<typeof searchSchema>;

export function playbooksSearch(
  search: Record<string, unknown>,
): PlaybooksSearch {
  return searchSchema.parse(search);
}

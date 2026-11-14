# Errata: 11_workspace_read_update_settings

Where the implementation of package 11
(`.specs/11_workspace_read_update_settings`) differs from what the package
says, or decides what it leaves open.

## Moving a workspace: what the package leaves open

- `org_id: null` moves the workspace to its owner's personal org, the org
  `identity.PersonalOrg` finds (see the erratum of package 10). When the owner
  has none, the update answers `400 invalid_request` with
  `org_id: the workspace's owner has no personal organization`, as a create
  does for its caller.
- The forks that move with their parent get the new `org_id` only: their
  `updated_at` is left as it was, since nothing of the fork itself was
  updated. The parent's `updated_at` is set as for every update.
- The owner-membership check runs whenever `org_id` is given, also when it
  names the workspace's current org, so an owner who left that org cannot
  name it again.

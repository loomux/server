### Changed

- A usage-limit attention's reset text is `attention.resets`, not
  `attention.resets_at` (API v1 freeze review, item 11): it is the agent's
  own words ("5pm (Europe/Istanbul)"), not a timestamp like the API's
  other `*_at` fields. Tasks stored before still read.

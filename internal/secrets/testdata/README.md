# Test fixtures: secrets.schema.json

**Temporary.** `secrets.schema.vps.json` and `secrets.schema.docker-local.json` are
what `SecretSchema.toJson` (`scripts/gen-env.scala` in the `versola` repository, after
PR #515 and #544, schema revision 1) prints for `vps` and `docker-local`. They were
reproduced mechanically from its `specs` list, in the same field order and layout,
because a published `versola-tools` that writes the file does not exist yet.

Replace them with the real file as soon as one does:

    docker run --rm -e TARGET=vps -e AUTH_URL=https://example.invalid \
      -e POSTGRES_HOST=127.0.0.1:5432 -v "$PWD/out:/out" \
      ghcr.io/versolauth/versola-tools:<version>      # then copy out/secrets.schema.json

(`TARGET=docker-local` for the other one.) `TestContractRealSchemaFiles` fails if
the CLI's reading of them drifts from what versola-tools writes.

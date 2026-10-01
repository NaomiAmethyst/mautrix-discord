# Legacy implementation

This directory preserves the bridgev1 implementation, including its config,
database migrations, formatter, direct media, QR login, and user-token support.
It has its own Go module and is excluded from the bridgev2 build and tests.

The root of the repository builds the bridgev2 bot/webhook relay implementation.
It uses a fresh database and registration. Do not point it at a bridgev1 database.
The old setup instructions and feature list describe this legacy implementation.

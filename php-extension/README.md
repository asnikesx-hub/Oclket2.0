# Oclket PHP extension

Native PHP extension for PHP 8.5 that accesses Oclket through its local Unix
socket. It implements only the local `L_GET`, `L_SET`, and `L_DEL` commands;
it does not expose peer, authentication, or replication commands.

## PHP API

```php
oclket_set(string $key, string $value, int $ttl = 0): bool
oclket_get(string $key): string|false
oclket_del(string $key): bool
```

Keys and values are binary-safe PHP strings. `ttl` is in seconds; `0` means no
expiration. `oclket_get()` returns `false` for a missing key and a string for a
found key, including an empty string. SET and DEL return `true` after the
service confirms the operation. Invalid arguments and socket/service errors
throw `Exception`.

The extension uses one persistent connection per PHP process and reconnects
on the next call after a transport failure. It does not retry an operation
whose response was lost, because the service may already have applied it.
Connection I/O has a five-second timeout.

## Build and install

Build on Linux with PHP 8.5 development headers and `phpize` installed:

```sh
cd php-extension
phpize
./configure --enable-oclket
make
sudo make install
```

Enable the module in the PHP configuration:

```ini
extension=oclket.so
oclket.socket_path=/tmp/oclket.sock
```

The PHP worker must be able to access the same Unix socket path as the Oclket
service. The path can also be changed at runtime with `ini_set()`.

## Example

```php
oclket_set("greeting", "hello", 60);
$value = oclket_get("greeting");
oclket_del("greeting");
```

With the extension enabled and the Oclket service running, run
`php smoke.php` to check missing keys, binary values, empty values, and deletes.

## Allocation behavior

The extension does not build temporary request payloads: it sends PHP string
data directly with vectored socket I/O and keeps its socket buffers on the
stack. GET reads the result directly into the PHP return string, avoiding an
additional intermediate copy. PHP must still allocate the returned string;
exceptions also allocate when reporting errors, so a literal zero-allocation
GET/SET API is not possible with ordinary PHP strings.

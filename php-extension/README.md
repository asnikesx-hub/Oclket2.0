# Oclket PHP extension

Native PHP extension for PHP 8.5 that accesses Oclket through its local Unix
socket. It implements only the local `L_GET`, `L_SET`, and `L_DEL` commands;
it does not expose peer, authentication, or replication commands.

## PHP API

```php
oclket_set(string $key, mixed $value, int $ttl = 0): bool
oclket_get(string $key): mixed
oclket_del(string $key): bool
```

Keys are binary-safe PHP strings. Values are encoded by the extension's
dependency-free binary codec and sent to Oclket as bytes. The codec supports
strings, integers, floats, booleans, `null`, arrays, and ordinary user-defined
objects with public properties. Object classes must already be loaded when
reading. Cyclic values, resources, special/internal objects, and objects with
private or protected properties or custom serialization methods are rejected.
Object identity and PHP references are not preserved. Values written by older
extension versions or clients without the binary marker keep the previous
string behavior; values written by the previous PHP-serialization version are
still decoded for compatibility.

`oclket_get()` returns `false` for a missing key, which is indistinguishable
from a stored `false`. `ttl` is in seconds; `0` means no expiration. SET and
DEL return `true` after the service confirms the operation. Invalid arguments
and socket/service errors throw `Exception`.

Only use object values from trusted clients: restoring user-defined objects may
run their magic property handlers or destructors. Legacy PHP-serialized values
may also invoke `__wakeup()` or `__unserialize()`. Restrict Unix socket
permissions to trusted local users.

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
oclket_set("greeting", ["message" => "hello", "count" => 3], 60);
$value = oclket_get("greeting");
oclket_del("greeting");
```

With the extension enabled and the Oclket service running, run
`php smoke.php` to check scalar, array, object, binary, and null values, plus
missing keys and deletes.

## Allocation behavior

The extension sends the encoded byte buffer with vectored socket I/O and keeps
its socket buffers on the stack. The codec uses `smart_str` growth for SET and
decodes GET values from the received payload buffer.

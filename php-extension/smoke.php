<?php

$key = 'oclket-php-smoke-' . bin2hex(random_bytes(12));
$value = "\0binary\0value\xff";

try {
    if (oclket_get($key) !== false) {
        throw new RuntimeException('Expected an initially missing key');
    }
    if (oclket_set($key, $value) !== true) {
        throw new RuntimeException('SET did not return true');
    }
    if (oclket_get($key) !== $value) {
        throw new RuntimeException('GET did not preserve the binary value');
    }
    if (oclket_set($key, '') !== true || oclket_get($key) !== '') {
        throw new RuntimeException('Empty values were not preserved');
    }
    if (oclket_del($key) !== true || oclket_get($key) !== false) {
        throw new RuntimeException('DEL did not remove the key');
    }
} finally {
    oclket_del($key);
}

echo "Oclket extension smoke test passed\n";

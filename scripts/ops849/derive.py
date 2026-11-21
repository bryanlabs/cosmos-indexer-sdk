"""Address derivations used by the OPS-849 label set (Cosmos SDK and ibc-go rules)."""
import hashlib

CHARSET = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"


def _polymod(values):
    gen = [0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3]
    chk = 1
    for v in values:
        b = chk >> 25
        chk = (chk & 0x1FFFFFF) << 5 ^ v
        for i in range(5):
            chk ^= gen[i] if ((b >> i) & 1) else 0
    return chk


def _hrp_expand(hrp):
    return [ord(x) >> 5 for x in hrp] + [0] + [ord(x) & 31 for x in hrp]


def _convertbits(data, frombits, tobits, pad=True):
    acc = bits = 0
    ret = []
    maxv = (1 << tobits) - 1
    for value in data:
        acc = (acc << frombits) | value
        bits += frombits
        while bits >= tobits:
            bits -= tobits
            ret.append((acc >> bits) & maxv)
    if pad and bits:
        ret.append((acc << (tobits - bits)) & maxv)
    elif not pad and (bits >= frombits or ((acc << (tobits - bits)) & maxv)):
        raise ValueError("invalid padding")
    return ret


def encode(hrp, raw: bytes) -> str:
    data = _convertbits(raw, 8, 5)
    poly = _polymod(_hrp_expand(hrp) + data + [0] * 6) ^ 1
    chk = [(poly >> 5 * (5 - i)) & 31 for i in range(6)]
    return hrp + "1" + "".join(CHARSET[d] for d in data + chk)


def decode(addr: str):
    hrp, _, rest = addr.rpartition("1")
    data = [CHARSET.index(c) for c in rest]
    if _polymod(_hrp_expand(hrp) + data) != 1:
        raise ValueError("bad checksum")
    return hrp, bytes(_convertbits(data[:-6], 5, 8, False))


def sdk_hash(typ: str, key: bytes) -> bytes:
    """cosmos-sdk types/address.Hash."""
    th = hashlib.sha256(typ.encode()).digest()
    return hashlib.sha256(th + key).digest()


def module_address(name: str) -> bytes:
    """authtypes.NewModuleAddress: first 20 bytes of sha256(name)."""
    return hashlib.sha256(name.encode()).digest()[:20]


def module_derived(module: str, key: bytes) -> bytes:
    """cosmos-sdk types/address.Module(module, key) for a single derivation key."""
    mkey = module.encode() + b"\x00" + key
    return sdk_hash("module", mkey)


def derive(addr: bytes, key: bytes) -> bytes:
    """cosmos-sdk types/address.Derive."""
    return hashlib.sha256(hashlib.sha256(addr).digest() + key).digest()


def ica_host_address(hrp: str, connection_id: str, controller_port: str) -> str:
    """ibc-go GenerateAddress: Derive(ModuleAddress("interchainaccounts"), connection+port)."""
    return encode(hrp, derive(module_address("interchainaccounts"), (connection_id + controller_port).encode()))


def ics20_escrow(hrp: str, port: str, channel: str) -> str:
    return encode(hrp, hashlib.sha256(b"ics20-1\x00" + f"{port}/{channel}".encode()).digest()[:20])

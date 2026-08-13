# Engine Architecture Diagram

Referenced from [`dev.md`'s "Engine Abstraction" section](../dev.md#engine-abstraction).

```mermaid
flowchart TD
    profiles["games/*.json profiles"] --> loader["profile loader\n(games)"]
    loader --> registry["engine registry\n(engine)"]

    registry --> unreal["engine/unreal\nGVAS - fully implemented\n(Clair Obscur)"]
    registry --> larian["engine/larian\nLSPK - fully implemented\n(Baldur's Gate 3)"]
    registry --> reengine["engine/reengine\nRE Engine DSSS - fully implemented\n(Resident Evil 2)"]
    registry --> unityblb["engine/unityblb\ngzip+TLV - fully implemented\n(Subnautica, Subnautica: Below Zero)"]
```

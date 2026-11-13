This would allow users of AREDN and other OpenWrt-based routers to make use of HF modems (or any KISS-compatible modem) to advertise their presence over HF. The aim is to address the problem of a decentralised mesh communications network coming online without any means of reaching a bootstrap server.

There are several straightforward mesh-based chat systems that could also be advertised, allowing other users to connect and exchange messages to arrange TCP/IP WireGuard tunnels, even if the two stations have never previously established a connection.

Yggdrasil is probably too resource-hungry to run on most OpenWrt-based routers, but it is an excellent option for the initial rendezvous, as it allows connections to be established without manually exchanging keys beforehand, whilst still allowing trusted public keys to be pinned.

Yggdrasil could provide the initial connection to a DMZ hosting mesh chat services, where operators could exchange WireGuard public keys and establish the tunnels needed to bring up Babel routing adjacencies.

The overall idea is to provide a way for otherwise disconnected mesh networks to discover one another, make initial contact and establish trusted connections without relying on existing infrastructure.

Naturally, all RF transmissions must comply with the applicable radio regulations and licensing requirements in your country or territory.

JS8Call was selected due to the established global network that is already in operation. This allows us to benefit from an existing infrastructure and a widely adopted communication method.

KISS remains a secondary option, as several modulation methods are already widely used across specific territories. However, further work is required to establish a common advertisement standard that ensures interoperability between the different implementations.

The intention is to maintain flexibility across different radio environments whilst providing a consistent method for stations to advertise their presence and available services.


## CAUTION
On AREDN, the daemon operates under the shared *nobody* user account, in line with the approach taken by other AREDN services. Its local control socket and configuration file are protected only by the permissions associated with this account. As a result, any other compromised process running under the same *nobody* account could potentially enable beaconing. This change would take effect following the next restart, resulting in transmissions being made under the operator's callsign.

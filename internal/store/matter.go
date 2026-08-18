package store

// NativeMatterAppID is de app-id van de Matter-plugin.
//
// Stulp kent hem bij naam voor één ding: een systeemmelding toeschrijven aan de
// app waar hij vandaan komt. De fabric, de node-id's en de apparaten zijn van de
// plugin en staan in zijn eigen state -- Stulp bewaart dat als ondoorzichtige
// blob en kijkt er niet in.
const NativeMatterAppID = "com.stulp.matter"

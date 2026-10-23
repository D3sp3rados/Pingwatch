import SwiftUI

struct LoginView: View {
    @Environment(AppModel.self) private var model
    @AppStorage("lastServer") private var server = "app.pingwacht.de"
    @AppStorage("lastEmail") private var email = ""
    @State private var password = ""
    @State private var busy = false
    @State private var error: String?
    @FocusState private var pwFocused: Bool

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    VStack(spacing: 10) {
                        Image(systemName: "waveform.path.ecg")
                            .font(.system(size: 52, weight: .semibold))
                            .foregroundStyle(PW.accent)
                        Text("PingWacht").font(.largeTitle.bold())
                        Text("Mit deinem PingWacht-Server verbinden")
                            .font(.subheadline).foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 12)
                    .listRowBackground(Color.clear)
                }
                Section("Server") {
                    TextField("Adresse", text: $server)
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .submitLabel(.next)
                        .onSubmit { pwFocused = true }
                }
                Section("Zugang") {
                    TextField("E-Mail (Betreiber: admin)", text: $email)
                        .keyboardType(.emailAddress)
                        .textContentType(.username)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .submitLabel(.next)
                        .onSubmit { pwFocused = true }
                    SecureField("Passwort", text: $password)
                        .textContentType(.password)
                        .focused($pwFocused)
                        .submitLabel(.go)
                        .onSubmit { Task { await login() } }
                }
                if let error {
                    Section {
                        Label(error, systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.red)
                    }
                }
                Section {
                    Button {
                        Task { await login() }
                    } label: {
                        HStack {
                            Spacer()
                            if busy { ProgressView() } else { Text("Anmelden").bold() }
                            Spacer()
                        }
                    }
                    .disabled(busy || server.isEmpty || password.isEmpty)
                    Button {
                        Task { await passkeyLogin() }
                    } label: {
                        HStack {
                            Spacer()
                            Label("Mit Passkey anmelden", systemImage: "person.badge.key.fill")
                            Spacer()
                        }
                    }
                    .disabled(busy || server.isEmpty)
                } footer: {
                    Text("Gleiche Zugangsdaten wie im Dashboard. Danach fragt die App, ob sie Benachrichtigungen senden darf.")
                }
            }
        }
    }

    private func passkeyLogin() async {
        busy = true
        error = nil
        do {
            try await model.loginWithPasskey(server: server)
        } catch {
            if !isPasskeyCancel(error) {
                self.error = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
            }
        }
        busy = false
    }

    private func login() async {
        busy = true
        error = nil
        do {
            try await model.login(server: server, email: email, password: password)
            password = ""
        } catch {
            self.error = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
        }
        busy = false
    }
}
